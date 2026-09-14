package orchestrator

// turno_bargein.go wires Turno's duplex near/far model into two uses:
//
//  1. Continuous VAD shadow logging — Turno's VAD head runs on every
//     frame of every call (not just during a pending barge-in) purely for
//     comparison against the existing Silero-based ms.vad. It never
//     influences behavior; see feedTurno's periodic "Turno VAD
//     shadow" log line.
//  2. Barge-in assist — while a tentative barge-in is open, Turno's
//     bargein score is tracked (peak-per-window) and read by
//     processUtterance to relax (never bypass) the MinWordsToInterrupt STT
//     gate when Turno corroborates a real interruption.
//
// (2) replaces an earlier design where a high-enough Turno score alone
// would confirmBargeInIfPending() directly, skipping STT validation
// entirely ("fast-confirm"). That was retired after root-causing why it had
// been disabled in production: the original model was trained only on
// OpenYAP/oto (independently-recorded dual-channel human conversation, no
// acoustic echo path between channels) and had never seen real device echo,
// so its confidence on leaked bot audio was little better than a guess. A
// retrain against Microsoft's AEC Challenge real-device recordings
// (turn-taking/checkpoints_v3_aec, see turn-taking/src/bench_aec_echo.py)
// cut the false-confirm rate on held-out real echo from 93.0% to 11.4% — a
// genuine improvement — but the same retrain cost true-confirm speed (real
// interruption median confirm time 140ms -> 3.46s) and recall (100% ->
// 84.8%), and a threshold/debounce sweep confirmed that's a hard tradeoff in
// this model's score distribution, not a tunable knob: no operating point
// recovers bypass-grade speed. A "fast" path that takes 3.46s no longer
// beats just waiting for STT, so this isn't a bypass anymore — it's a
// corroborating signal for the STT gate that's already running regardless,
// which doesn't need to be fast to be useful.
//
// Both are purely additive: when ms.turno is nil (TurnoModelPath
// unset, or the model failed to load), feedTurno is a no-op.

import "encoding/binary"

const turnoSampleRate = 16000
const turnoFrameBytes = 320 * 2 // Hop samples * 2 bytes/sample (int16 PCM)

// turnoFarEndBufCap caps the far-end ring buffer at 2s of 16kHz PCM16 — plenty
// of slack for the near/far alignment to be approximate (this is a coarse
// relational signal, not sample-accurate echo cancellation) without letting
// the buffer grow unbounded across a long bot utterance.
const turnoFarEndBufCap = 2 * turnoSampleRate * 2 // bytes

// turnoShadowLogEveryNFrames bounds the VAD-shadow diagnostic log to about once
// per second of audio (20ms/frame) regardless of call volume — this runs for
// the full duration of every call, unlike the bargein logging below, which
// is naturally bounded to pending-barge-in windows only.
const turnoShadowLogEveryNFrames = 50

// turnoTurnStateVADFloor is the speech probability below which a frame's
// turn-completion verdict is ignored. The heads are trained to describe the
// turn being spoken; sampling them from the trailing silence after the user
// stops would record a verdict about silence and compare it against a
// transcript, which is not the comparison we want to draw conclusions from.
const turnoTurnStateVADFloor = 0.5

// noteFarEndAudio appends the bot's own outgoing audio (already resampled
// to 16kHz PCM16 by the caller) to the far-end ring buffer that feedTurno
// reads from. No-op if Turno isn't loaded — no reason to hold onto audio
// nothing will ever read.
func (ms *ManagedStream) noteFarEndAudio(pcm16kHz []byte) {
	if ms.turno == nil || len(pcm16kHz) == 0 {
		return
	}
	ms.farEndMu.Lock()
	ms.farEndBuf = append(ms.farEndBuf, pcm16kHz...)
	if excess := len(ms.farEndBuf) - turnoFarEndBufCap; excess > 0 {
		ms.farEndBuf = ms.farEndBuf[excess:]
	}
	ms.farEndMu.Unlock()
}

// takeFarEndFrame pops up to turnoFrameBytes of far-end audio, oldest first.
// Returns nil (meaning "treat as silence") if none is buffered — the bot
// may simply not be speaking right now, which is a normal, valid input to
// the duplex gate (near energy with no far energy is exactly what should
// read as "not an overlap at all").
func (ms *ManagedStream) takeFarEndFrame() []byte {
	ms.farEndMu.Lock()
	defer ms.farEndMu.Unlock()
	if len(ms.farEndBuf) < turnoFrameBytes {
		return nil
	}
	frame := make([]byte, turnoFrameBytes)
	copy(frame, ms.farEndBuf[:turnoFrameBytes])
	ms.farEndBuf = ms.farEndBuf[turnoFrameBytes:]
	return frame
}

// pcm16ToFloat32 converts little-endian int16 PCM to float32 in [-1, 1],
// matching turno.Hop-sized frames.
func pcm16ToFloat32(pcm []byte) []float32 {
	out := make([]float32, len(pcm)/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		out[i] = float32(v) / 32768.0
	}
	return out
}

// feedTurno is called from handleAudio with each incoming chunk of
// near-end (user mic) audio, already resampled to 16kHz. It runs for the
// full duration of every call — not just while a barge-in is tentatively
// open — so the VAD shadow comparison has continuous data, not just samples
// from windows Silero already flagged.
func (ms *ManagedStream) feedTurno(nearChunk16k []byte) {
	if ms.turno == nil || len(nearChunk16k) == 0 {
		return
	}

	ms.mu.Lock()
	pending := ms.pendingBargeIn
	gen := ms.pendingBargeGen
	ms.mu.Unlock()

	ms.turnoNearAccum = append(ms.turnoNearAccum, nearChunk16k...)
	for len(ms.turnoNearAccum) >= turnoFrameBytes {
		frame := ms.turnoNearAccum[:turnoFrameBytes]
		ms.turnoNearAccum = ms.turnoNearAccum[turnoFrameBytes:]

		nearF := pcm16ToFloat32(frame)
		var farF []float32
		if far := ms.takeFarEndFrame(); far != nil {
			farF = pcm16ToFloat32(far)
		}

		decision, err := ms.turno.Step(nearF, farF)
		if err != nil {
			ms.logger.Warn("Turno inference error", "error", err)
			return
		}

		// Turn-completion heads come from the SEPARATE v6 instance, not from
		// `decision` above. The gating model's horizon head is dead (max
		// p_end_200ms 0.062 on real speech — it cannot cross any threshold),
		// so reading these off it would produce a dataset that looks fine and
		// means nothing. v6 is signature-identical, so the same frame feeds
		// both; only its TurnState/Horizon are kept and its VAD/bargein are
		// discarded. Gating still reads `decision` (v3) exclusively.
		if ms.turnoTurn != nil {
			turnDec, terr := ms.turnoTurn.Step(nearF, farF)
			if terr != nil {
				ms.logger.Warn("Turno turn-completion inference error", "error", terr)
			} else if turnDec.VAD >= turnoTurnStateVADFloor {
				// Gate on the shadow model's own VAD: it is the model whose
				// heads we are recording, so its notion of "mid-speech" is the
				// one that makes the verdict meaningful.
				ms.mu.Lock()
				ms.turnoLastTurnState = turnDec.TurnState
				ms.turnoLastHorizon = turnDec.Horizon
				ms.turnoLastTurnLabel = turnDec.TurnStateLabel()
				ms.turnoTurnStateFrames++
				ms.mu.Unlock()
			}
		}

		// VAD shadow comparison: purely observability, zero effect on
		// behavior. Silero (ms.vad) remains the only thing that gates
		// anything here. Bounded to ~1 line/sec/call regardless of volume.
		ms.turnoVadDiagFrames++
		if ms.turnoVadDiagFrames%turnoShadowLogEveryNFrames == 0 {
			ms.logger.Info("Turno VAD shadow",
				"turno_vad", decision.VAD,
				"silero_speaking", ms.vad != nil && ms.vad.IsSpeaking())
		}

		if !pending {
			continue
		}

		// Re-check pending/gen each frame: STT can confirm/resolve the
		// pending barge-in at any point, and a stale generation must not
		// keep contributing to a peak score no one is evaluating anymore.
		ms.mu.Lock()
		stillPending := ms.pendingBargeIn && ms.pendingBargeGen == gen
		ms.mu.Unlock()
		if !stillPending {
			continue
		}

		// Score visibility while tuning against real traffic: bounded to
		// open pending-barge-in windows only, typically well under a
		// couple seconds of audio.
		ms.logger.Info("Turno frame", "bargein", decision.Bargein, "vad", decision.VAD)

		ms.mu.Lock()
		if decision.Bargein > ms.turnoBargeinPeakScore {
			ms.turnoBargeinPeakScore = decision.Bargein
		}
		ms.mu.Unlock()
	}
}
