package orchestrator

import "math"

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

	// echoFarEndBufCap caps the non-destructive far-end copy at 2s of 16kHz PCM16, the same
	// window turno_bargein.go carries — long enough to cover any plausible speaker-to-mic-to-STT
	// round trip on a browser call (dominated by network + audio-buffer latency, not propagation
	// delay, since there is no physical distance worth measuring on a laptop).
	echoFarEndBufCap = 2 * 16000 * 2

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

// noteFarEndEcho appends the bot's own outgoing audio (already resampled to 16kHz PCM16 by the
// caller) to the non-destructive far-end copy. Called unconditionally from emitFrames, unlike
// Turno's noteFarEndAudio which only runs when ms.turno is loaded — this mechanism doesn't depend
// on Turno at all.
func (ms *ManagedStream) noteFarEndEcho(pcm16kHz []byte) {
	if len(pcm16kHz) == 0 {
		return
	}
	ms.echoMu.Lock()
	ms.echoFarEndBuf = append(ms.echoFarEndBuf, pcm16kHz...)
	if excess := len(ms.echoFarEndBuf) - echoFarEndBufCap; excess > 0 {
		ms.echoFarEndBuf = ms.echoFarEndBuf[excess:]
	}
	ms.echoMu.Unlock()
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
	far := make([]byte, len(ms.echoFarEndBuf))
	copy(far, ms.echoFarEndBuf)
	ms.echoMu.Unlock()

	nearEnv := rmsEnvelope(near, echoFrameBytes)
	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearFrames = len(nearEnv)
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
