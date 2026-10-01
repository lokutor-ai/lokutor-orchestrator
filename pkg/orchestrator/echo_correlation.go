package orchestrator

import (
	"math"
	"time"
)

// echo_correlation.go: amplitude/timing acoustic-echo detection for the barge-in confirmation
// gate, replacing text-transcript similarity (the former isLikelyEcho, compared against
// ms.lastResponseText) as the live gate.
//
// Why not keep the text check: on a website there is no telephony hop, but the report that
// motivated this was still the bot cutting itself off and restarting mid-greeting — production
// logs from the same window showed the identical "Hola." echo caught by the text check for some
// simultaneous sessions and NOT caught for others. The text check depends on STT transcribing the
// echoed audio well enough to score >=60% word overlap against what the bot said; on a short
// utterance (the opening greeting is 5-7 words), one or two STT errors on a bleed-through echo —
// network/codec artifacts, a second STT pass on already-synthesized audio — is enough to drop
// below that bar. It also cannot fire at all until STT produces a transcript.
//
// Why not Turno's own bargein score instead: turno_bargein.go's header documents that history in
// full. The model was retrained specifically on real device echo (Microsoft's AEC Challenge
// corpus) and the false-confirm rate on held-out echo fell from 93.0% to 11.4% — a real
// improvement — but recall on genuine interruptions fell from 100% to 84.8% in the same retrain,
// and that's documented as a hard tradeoff in the model's score distribution, not a tunable knob.
// Using a LOW Turno score to veto a candidate interrupt would silently eat roughly 15% of real
// "wait"/"stop"s along with the echoes — an unquantified cost for a symptom (self-interruption)
// that is jarring but recoverable, traded against callers whose real interruption gets ignored,
// which is worse. Turno's score stays as a corroborating signal that can only lower the bar, not
// raise it against the caller.
//
// What this does instead: a direct claim, not an inference from either text or a trained score —
// "is this incoming audio explainable as a delayed, attenuated copy of audio we know we just
// played." That is exactly what acoustic echo is, mechanically. It runs on the raw amplitude
// envelope (RMS per 20ms frame, matching Turno's own hop), which needs neither STT nor a model,
// and works even before a transcript exists.
const (
	// echoFrameBytes is 20ms at 16kHz PCM16 — matches turno_bargein.go's own hop so the two
	// systems reason about audio in the same-sized slices, though they never share a buffer.
	echoFrameBytes = 320 * 2

	// echoFarEndBufCap caps the far-end copy at 12s of 16kHz PCM16. It holds what the caller's
	// speaker has played, in real time (see advanceFarEndEcho), so it has to be at least as long as
	// the near-end window it is compared with — the whole pending barge-in, which can run for a few
	// seconds — plus the echo's delay.
	echoFarEndBufCap = 12 * 16000 * 2

	// echoMaxLagFrames bounds how far behind the speaker the microphone may hear it: 1.5s of
	// 20ms frames covers a Bluetooth sink (~0.35s) plus a browser's playout buffering and network
	// jitter. Searching every alignment of a long far-end window instead let an unrelated older
	// stretch of the agent's speech match by chance.
	echoMaxLagFrames = 75

	// echoMinNearFrames is the shortest near-end snapshot worth correlating at all. Below three
	// 20ms frames (60ms) a correlation coefficient is noise, not a measurement — a real decision
	// needs enough samples for "these two envelopes rise and fall together" to mean something.
	echoMinNearFrames = 3

	// echoCorrelationThreshold is deliberately conservative for a first production measurement.
	// Every decision is logged with its score regardless of outcome (see isLikelyAcousticEcho),
	// the same way isLikelyEcho's own 0.8->0.6 history was arrived at — tune this against real
	// traffic, not in the abstract. A correlation this high is a strong claim: true echo is
	// literally the same signal, delayed and attenuated, so a genuine match should sit well above
	// where two independent speech signals (bot talking, caller coincidentally talking) would ever
	// land by chance.
	echoCorrelationThreshold = 0.75
)

// echoFarChunk is outgoing audio handed to the transport, not yet played: start is when the
// caller's playout clock (spoken_truth.go) says it starts playing.
type echoFarChunk struct {
	gen   int
	start time.Time
	pcm   []byte // 16kHz PCM16
}

// noteFarEndEchoScheduled queues outgoing audio for the echo check at the time it will PLAY, not
// the time it was sent. Synthesis runs several times faster than real time, so the audio sent
// most recently is seconds ahead of what the speaker is playing; the far-end copy used to be
// "the last 2s sent", which on any reply longer than that held audio the caller had not heard yet
// while the audio actually playing had already been dropped, and the correlation compared the
// microphone with the wrong speech (production scores sat near 0 on real echo). Called where
// emitWithGen puts a chunk on the playout clock, with the time that chunk starts playing.
func (ms *ManagedStream) noteFarEndEchoScheduled(gen int, pcm16kHz []byte, start time.Time) {
	if len(pcm16kHz) == 0 {
		return
	}
	ms.echoMu.Lock()
	// A new generation replaces what the previous one still had queued: the transport drops it.
	kept := ms.echoFarPending[:0]
	for _, c := range ms.echoFarPending {
		if c.gen >= gen {
			kept = append(kept, c)
		}
	}
	ms.echoFarPending = append(kept, echoFarChunk{gen: gen, start: start, pcm: pcm16kHz})
	ms.echoMu.Unlock()
}

// dropUnplayedFarEndEcho forgets queued audio that will not play after now: the transport was told
// to stop, or discards its queue at a barge-in (the two places the playout clock is cut).
func (ms *ManagedStream) dropUnplayedFarEndEcho(now time.Time) {
	ms.echoMu.Lock()
	kept := ms.echoFarPending[:0]
	for _, c := range ms.echoFarPending {
		if c.start.Before(now) {
			played := int(now.Sub(c.start).Seconds()*16000) * 2
			if played < len(c.pcm) {
				c.pcm = c.pcm[:played]
			}
			kept = append(kept, c)
		}
	}
	ms.echoFarPending = kept
	ms.echoMu.Unlock()
}

// advanceFarEndEcho moves what the speaker has played up to now from the queue into echoFarEndBuf,
// with silence wherever nothing was playing, so echoFarEndBuf mirrors the speaker in real time and
// ends at the same instant as the microphone audio fed alongside it (handleAudio calls this for
// every microphone frame). The first call only starts the clock.
func (ms *ManagedStream) advanceFarEndEcho(now time.Time) {
	ms.echoMu.Lock()
	defer ms.echoMu.Unlock()
	if ms.echoFarClock.IsZero() {
		ms.echoFarClock = now
		return
	}
	maxSpan := time.Duration(echoFarEndBufCap/2) * time.Second / 16000
	if now.Sub(ms.echoFarClock) > maxSpan {
		ms.echoFarClock = now.Add(-maxSpan)
	}
	bytesFor := func(d time.Duration) int { return int(d.Seconds()*16000) * 2 }
	durOf := func(n int) time.Duration { return time.Duration(n/2) * time.Second / 16000 }
	for ms.echoFarClock.Before(now) {
		if len(ms.echoFarPending) == 0 {
			n := bytesFor(now.Sub(ms.echoFarClock))
			if n == 0 {
				break
			}
			ms.echoFarEndBuf = append(ms.echoFarEndBuf, make([]byte, n)...)
			ms.echoFarClock = ms.echoFarClock.Add(durOf(n))
			break
		}
		c := &ms.echoFarPending[0]
		if c.start.After(ms.echoFarClock) {
			gapEnd := c.start
			if gapEnd.After(now) {
				gapEnd = now
			}
			n := bytesFor(gapEnd.Sub(ms.echoFarClock))
			if n == 0 {
				// Less than one sample of gap: the chunk starts now.
				c.start = ms.echoFarClock
				continue
			}
			ms.echoFarEndBuf = append(ms.echoFarEndBuf, make([]byte, n)...)
			ms.echoFarClock = ms.echoFarClock.Add(durOf(n))
			continue
		}
		off := bytesFor(ms.echoFarClock.Sub(c.start))
		if off >= len(c.pcm) {
			ms.echoFarPending = ms.echoFarPending[1:]
			continue
		}
		n := len(c.pcm) - off
		if want := bytesFor(now.Sub(ms.echoFarClock)); want < n {
			n = want
		}
		if n == 0 {
			break
		}
		ms.echoFarEndBuf = append(ms.echoFarEndBuf, c.pcm[off:off+n]...)
		ms.echoFarClock = ms.echoFarClock.Add(durOf(n))
		if off+n >= len(c.pcm) {
			ms.echoFarPending = ms.echoFarPending[1:]
		}
	}
	if excess := len(ms.echoFarEndBuf) - echoFarEndBufCap; excess > 0 {
		ms.echoFarEndBuf = ms.echoFarEndBuf[excess:]
	}
}

// resetNearEndEcho clears the near-end accumulator and pins it to gen, the response generation
// active when this tentative barge-in opened. Call this at the same point pendingBargeIn is set —
// see onVADStart.
func (ms *ManagedStream) resetNearEndEcho(gen int) {
	ms.echoMu.Lock()
	ms.echoNearEndBuf = nil
	ms.echoNearEndGen = gen
	ms.echoMu.Unlock()
}

// noteNearEndEcho appends near-end (caller mic) audio, already resampled to 16kHz PCM16, ONLY
// when gen still matches the pending barge-in this accumulator was reset for — a stale call
// racing in after a newer turn started must not contaminate that newer turn's decision.
func (ms *ManagedStream) noteNearEndEcho(pcm16kHz []byte, gen int) {
	if len(pcm16kHz) == 0 {
		return
	}
	ms.echoMu.Lock()
	if ms.echoNearEndGen == gen {
		ms.echoNearEndBuf = append(ms.echoNearEndBuf, pcm16kHz...)
	}
	ms.echoMu.Unlock()
}

// rmsEnvelope reduces 16kHz PCM16 audio to one RMS value per frameBytes-sized frame. Trailing
// samples shorter than a full frame are dropped rather than padded — a partial frame's RMS would
// be systematically biased toward whatever silence padding contributed.
func rmsEnvelope(pcm16kHz []byte, frameBytes int) []float64 {
	n := len(pcm16kHz) / frameBytes
	out := make([]float64, n)
	for f := 0; f < n; f++ {
		frame := pcm16kHz[f*frameBytes : (f+1)*frameBytes]
		samples := len(frame) / 2
		var sumSq float64
		for i := 0; i < samples; i++ {
			s := int16(frame[i*2]) | int16(frame[i*2+1])<<8
			v := float64(s) / 32768.0
			sumSq += v * v
		}
		out[f] = math.Sqrt(sumSq / float64(samples))
	}
	return out
}

// pearson is the standard normalized cross-correlation coefficient between two equal-length
// series, in [-1, 1]. Returns 0 if either series has zero variance (silence, or a single constant
// value) rather than dividing by zero — flat audio correlates with nothing, including itself.
func pearson(a, b []float64) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var sumA, sumB float64
	for i := 0; i < n; i++ {
		sumA += a[i]
		sumB += b[i]
	}
	meanA, meanB := sumA/float64(n), sumB/float64(n)
	var num, denA, denB float64
	for i := 0; i < n; i++ {
		da, db := a[i]-meanA, b[i]-meanB
		num += da * db
		denA += da * da
		denB += db * db
	}
	if denA <= 0 || denB <= 0 {
		return 0
	}
	return num / math.Sqrt(denA*denB)
}

// correlateEnvelopes searches every alignment of near against far and returns the strongest
// match found, plus the far-end frame index it started at. near can only match a PAST stretch of
// far (an echo follows what produced it, never precedes it), which searching start from 0 up
// already guarantees — far is oldest-first, so a smaller start is an OLDER, longer-lag match.
// Returns peak 0 and atFarFrame -1 if far is shorter than near (nothing to search).
func correlateEnvelopes(near, far []float64) (peak float64, atFarFrame int) {
	atFarFrame = -1
	if len(near) == 0 || len(far) < len(near) {
		return 0, -1
	}
	for start := 0; start+len(near) <= len(far); start++ {
		c := pearson(near, far[start:start+len(near)])
		if c > peak {
			peak = c
			atFarFrame = start
		}
	}
	return peak, atFarFrame
}

// isLikelyAcousticEcho decides whether the near-end audio captured during the current pending
// barge-in window (gen) is well-explained as a delayed, attenuated copy of the bot's own recent
// output. nearFrames is returned for logging even on an early "not enough data" exit, so a run of
// echoMinNearFrames near-misses is visible rather than looking identical to "never ran".
func (ms *ManagedStream) isLikelyAcousticEcho(gen int) (echo bool, score float64, lagMs int, nearFrames int) {
	ms.echoMu.Lock()
	if ms.echoNearEndGen != gen {
		ms.echoMu.Unlock()
		return false, 0, 0, 0
	}
	near := make([]byte, len(ms.echoNearEndBuf))
	copy(near, ms.echoNearEndBuf)
	// Only the far end's last len(near)+echoMaxLagFrames frames can align with the near end (see
	// below), so only they are copied: this runs on every microphone frame of a pending barge-in,
	// under the stream lock when stopPlayThroughIfDue asks, and the far end holds 12 s. The cut is
	// a whole number of frames from the start, so the frames kept are the same ones.
	farSrc := ms.echoFarEndBuf
	if skip := len(farSrc)/echoFrameBytes - (len(near)/echoFrameBytes + echoMaxLagFrames); skip > 0 {
		farSrc = farSrc[skip*echoFrameBytes:]
	}
	far := make([]byte, len(farSrc))
	copy(far, farSrc)
	ms.echoMu.Unlock()

	nearEnv := rmsEnvelope(near, echoFrameBytes)
	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearFrames = len(nearEnv)
	// Both buffers end at the latest microphone frame, so the echo can only sit up to
	// echoMaxLagFrames back from the far end's tail.
	if maxFar := len(nearEnv) + echoMaxLagFrames; len(farEnv) > maxFar {
		farEnv = farEnv[len(farEnv)-maxFar:]
	}
	if len(nearEnv) < echoMinNearFrames || len(farEnv) < len(nearEnv) {
		return false, 0, 0, nearFrames
	}

	peak, atFrame := correlateEnvelopes(nearEnv, farEnv)
	if atFrame < 0 {
		return false, peak, 0, nearFrames
	}
	// atFrame counts from the oldest end of farEnv. The most recent possible alignment is
	// len(farEnv)-len(nearEnv); the gap between that and where the match actually landed is how
	// much extra delay (beyond zero) the echo carried.
	lagFrames := (len(farEnv) - len(nearEnv)) - atFrame
	if lagFrames < 0 {
		lagFrames = 0
	}
	return peak >= echoCorrelationThreshold, peak, lagFrames * 20, nearFrames
}
