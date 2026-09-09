package orchestrator

// gateturn_bargein.go wires GateTurn's duplex near/far barge-in classifier
// into the existing tentative-barge-in flow (see managed_stream.go's
// pendingBargeIn/confirmBargeInIfPending/resolvePendingBargeIn). It is
// purely additive: when ms.gateturn is nil (GateTurnModelPath unset, or the
// model failed to load), every function here is a no-op and behavior is
// identical to before this file existed.
//
// Why this exists: today, once VAD opens a tentative barge-in, the
// only way to commit to it or roll it back is to wait for STT to transcribe
// enough of the interrupting audio. GateTurn's bargein head is trained
// specifically to tell a real interruption from a backchannel ("mhm") from
// the near/far energy dynamics alone, in the same 20ms frame — so a
// confident score lets us commit (or roll back) several hundred ms sooner
// than an STT round trip, without touching the STT path at all as a
// fallback for the frames where GateTurn isn't confident.

import "encoding/binary"

// gtConsecutiveFramesRequired is how many consecutive frames must agree
// before acting — a single 20ms frame's score is noisy, so require at
// least a blip-sized debounce. 2 (40ms) rather than 3 (60ms): the entire
// point of this fast path is shaving time off an STT round trip that's
// several hundred ms, so the debounce itself shouldn't be a meaningful
// fraction of that budget.
const gtConsecutiveFramesRequired = 2

// gtFarEndBufCap caps the far-end ring buffer at 2s of 16kHz PCM16 — plenty
// of slack for the near/far alignment to be approximate (this is a coarse
// relational signal, not sample-accurate echo cancellation) without letting
// the buffer grow unbounded across a long bot utterance.
const gtFarEndBufCap = 2 * gtSampleRate * 2 // bytes

const gtSampleRate = 16000
const gtFrameBytes = 320 * 2 // Hop samples * 2 bytes/sample (int16 PCM)

// noteFarEndAudio appends the bot's own outgoing audio (already resampled
// to 16kHz PCM16 by the caller) to the far-end ring buffer that
// feedGateTurnBargein reads from. No-op if GateTurn isn't loaded — no
// reason to hold onto audio nothing will ever read.
func (ms *ManagedStream) noteFarEndAudio(pcm16kHz []byte) {
	if ms.gateturn == nil || len(pcm16kHz) == 0 {
		return
	}
	ms.farEndMu.Lock()
	ms.farEndBuf = append(ms.farEndBuf, pcm16kHz...)
	if excess := len(ms.farEndBuf) - gtFarEndBufCap; excess > 0 {
		ms.farEndBuf = ms.farEndBuf[excess:]
	}
	ms.farEndMu.Unlock()
}

// takeFarEndFrame pops up to gtFrameBytes of far-end audio, oldest first.
// Returns nil (meaning "treat as silence") if none is buffered — the bot
// may simply not be speaking right now, which is a normal, valid input to
// the duplex gate (near energy with no far energy is exactly what should
// read as "not an overlap at all").
func (ms *ManagedStream) takeFarEndFrame() []byte {
	ms.farEndMu.Lock()
	defer ms.farEndMu.Unlock()
	if len(ms.farEndBuf) < gtFrameBytes {
		return nil
	}
	frame := make([]byte, gtFrameBytes)
	copy(frame, ms.farEndBuf[:gtFrameBytes])
	ms.farEndBuf = ms.farEndBuf[gtFrameBytes:]
	return frame
}

// pcm16ToFloat32 converts little-endian int16 PCM to float32 in [-1, 1],
// matching gateturn.Hop-sized frames.
func pcm16ToFloat32(pcm []byte) []float32 {
	out := make([]float32, len(pcm)/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		out[i] = float32(v) / 32768.0
	}
	return out
}

// feedGateTurnBargein is called from handleAudio with each
// incoming chunk of near-end (user mic) audio, already resampled to 16kHz.
// It only runs the model while a tentative barge-in is open — there is
// nothing for this fast path to decide otherwise, since confirm/resolve are
// meaningless without a pending barge-in to act on.
func (ms *ManagedStream) feedGateTurnBargein(nearChunk16k []byte) {
	if ms.gateturn == nil || len(nearChunk16k) == 0 {
		return
	}

	ms.mu.Lock()
	pending := ms.pendingBargeIn
	gen := ms.pendingBargeGen
	ms.mu.Unlock()

	if !pending {
		// Not mid-barge-in: nothing to decide. Drop any partial frame and
		// run counters so a stale accumulation doesn't leak into the next
		// real pending window (which may start mid-utterance at an
		// arbitrary byte offset relative to this one).
		if len(ms.gtNearAccum) > 0 {
			ms.gtNearAccum = ms.gtNearAccum[:0]
		}
		ms.gtConfirmRun = 0
		ms.gtResolveRun = 0
		return
	}

	ms.gtNearAccum = append(ms.gtNearAccum, nearChunk16k...)
	for len(ms.gtNearAccum) >= gtFrameBytes {
		frame := ms.gtNearAccum[:gtFrameBytes]
		ms.gtNearAccum = ms.gtNearAccum[gtFrameBytes:]

		nearF := pcm16ToFloat32(frame)
		var farF []float32
		if far := ms.takeFarEndFrame(); far != nil {
			farF = pcm16ToFloat32(far)
		}

		decision, err := ms.gateturn.Step(nearF, farF)
		if err != nil {
			ms.logger.Warn("GateTurn inference error", "error", err)
			return
		}
		// Score visibility while tuning against real traffic: the confirm/
		// resolve log lines below only fire once a run of frames commits,
		// which said nothing at all in the first hour of production calls
		// (see the commit that lowered the thresholds) — logging every
		// frame's raw score during an actual pending window is what makes
		// that observable instead of a guess. Volume is bounded: this only
		// runs while a tentative barge-in is open, typically well under a
		// couple seconds of audio.
		ms.logger.Info("GateTurn frame", "bargein", decision.Bargein, "vad", decision.VAD)

		// Re-check pending/gen each frame: the loop that confirms via STT
		// can win the race at any point, and a stale generation must not
		// act on a barge-in that's already resolved or superseded.
		ms.mu.Lock()
		stillPending := ms.pendingBargeIn && ms.pendingBargeGen == gen
		ms.mu.Unlock()
		if !stillPending {
			ms.gtConfirmRun = 0
			ms.gtResolveRun = 0
			return
		}

		switch {
		case decision.Bargein >= ms.gtBargeinConfirmThr:
			ms.gtConfirmRun++
			ms.gtResolveRun = 0
		case decision.Bargein <= ms.gtBargeinResolveThr:
			ms.gtResolveRun++
			ms.gtConfirmRun = 0
		default:
			ms.gtConfirmRun = 0
			ms.gtResolveRun = 0
		}

		if ms.gtConfirmRun >= gtConsecutiveFramesRequired {
			ms.logger.Info("GateTurn confirmed barge-in", "bargein", decision.Bargein)
			ms.gtConfirmRun = 0
			ms.confirmBargeInIfPending()
			return
		}
		if ms.gtResolveRun >= gtConsecutiveFramesRequired {
			ms.logger.Info("GateTurn resolved barge-in as backchannel", "bargein", decision.Bargein)
			ms.gtResolveRun = 0
			ms.resolvePendingBargeIn()
			return
		}
	}
}
