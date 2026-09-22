package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"
)

func tone(nSamples int, freq, amp float64) []byte {
	out := make([]byte, nSamples*2)
	for i := 0; i < nSamples; i++ {
		v := int16(amp * math.Sin(float64(i)*freq))
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

func attenuate(pcm []byte, factor float64) []byte {
	out := make([]byte, len(pcm))
	for i := 0; i < len(pcm)/2; i++ {
		s := int16(pcm[i*2]) | int16(pcm[i*2+1])<<8
		v := int16(float64(s) * factor)
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

func silence(nSamples int) []byte {
	return make([]byte, nSamples*2)
}

func TestRMSEnvelope_SilenceIsZero(t *testing.T) {
	env := rmsEnvelope(silence(320*5), echoFrameBytes)
	if len(env) != 5 {
		t.Fatalf("expected 5 frames, got %d", len(env))
	}
	for i, v := range env {
		if v != 0 {
			t.Errorf("frame %d: expected 0 RMS on silence, got %f", i, v)
		}
	}
}

func TestRMSEnvelope_DropsPartialTrailingFrame(t *testing.T) {
	// 2.5 frames worth of bytes -- the trailing half-frame must be dropped, not padded.
	pcm := tone(320*2+160, 0.3, 8000)
	env := rmsEnvelope(pcm, echoFrameBytes)
	if len(env) != 2 {
		t.Fatalf("expected 2 full frames (partial trailing frame dropped), got %d", len(env))
	}
}

func TestPearson_IdenticalSignalsCorrelatePerfectly(t *testing.T) {
	a := rmsEnvelope(tone(320*10, 0.3, 8000), echoFrameBytes)
	c := pearson(a, a)
	if c < 0.999 {
		t.Errorf("identical envelopes should correlate ~1.0, got %f", c)
	}
}

func TestPearson_ZeroVarianceReturnsZeroNotNaN(t *testing.T) {
	flat := []float64{0.5, 0.5, 0.5, 0.5}
	varying := []float64{0.1, 0.9, 0.2, 0.8}
	c := pearson(flat, varying)
	if c != 0 {
		t.Errorf("zero-variance series must correlate as 0, got %f", c)
	}
	if math.IsNaN(c) {
		t.Error("pearson must never return NaN")
	}
}

func TestPearson_MismatchedLengthsReturnZero(t *testing.T) {
	if c := pearson([]float64{1, 2, 3}, []float64{1, 2}); c != 0 {
		t.Errorf("mismatched lengths should return 0, got %f", c)
	}
}

func TestCorrelateEnvelopes_FindsAttenuatedEchoAtCorrectLag(t *testing.T) {
	// far = 10 frames of silence (nothing played yet in this window) followed by 5 frames of tone
	// (what the bot just said), for 15 frames total. near = a quieter copy of the SAME 5-frame
	// tone, i.e. what the mic picked up as echo, arriving "now" -- so it should best-match the
	// last 5 frames of far, at lag 0.
	toneFrames := tone(320*5, 0.3, 8000)
	far := append(silence(320*10), toneFrames...)
	near := attenuate(toneFrames, 0.4)

	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearEnv := rmsEnvelope(near, echoFrameBytes)

	peak, atFrame := correlateEnvelopes(nearEnv, farEnv)
	if peak < echoCorrelationThreshold {
		t.Fatalf("expected a strong correlation for an attenuated echo, got %f", peak)
	}
	wantAtFrame := len(farEnv) - len(nearEnv) // the most recent possible alignment = lag 0
	if atFrame != wantAtFrame {
		t.Errorf("expected best match at frame %d (lag 0), got %d", wantAtFrame, atFrame)
	}
}

func TestCorrelateEnvelopes_FindsEchoAtNonzeroLag(t *testing.T) {
	// The echo shows up 3 frames (60ms) after the far-end audio that produced it -- far has the
	// tone in the MIDDLE of the buffer, not at the very end, simulating network/buffering delay
	// between when the bot's audio was recorded here and when its echo actually reached the mic.
	toneFrames := tone(320*4, 0.3, 8000)
	far := append(append(silence(320*6), toneFrames...), silence(320*3)...)
	near := attenuate(toneFrames, 0.5)

	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearEnv := rmsEnvelope(near, echoFrameBytes)

	peak, atFrame := correlateEnvelopes(nearEnv, farEnv)
	if peak < echoCorrelationThreshold {
		t.Fatalf("expected a strong correlation, got %f", peak)
	}
	if atFrame != 6 {
		t.Errorf("expected the match to land at far-end frame 6 (where the tone starts), got %d", atFrame)
	}
}

func TestCorrelateEnvelopes_IndependentSpeechDoesNotFalsePositive(t *testing.T) {
	// Two DIFFERENT signals (different frequency, i.e. a different "voice") should not correlate
	// strongly just because both are speech-shaped energy -- this is the false-positive risk this
	// whole mechanism has to avoid: two independent speakers should not look like an echo of each
	// other.
	far := tone(320*10, 0.3, 8000)
	near := tone(320*5, 1.7, 6000) // different frequency AND amplitude
	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearEnv := rmsEnvelope(near, echoFrameBytes)

	peak, _ := correlateEnvelopes(nearEnv, farEnv)
	if peak >= echoCorrelationThreshold {
		t.Errorf("independent signals should not correlate above threshold, got %f", peak)
	}
}

func TestCorrelateEnvelopes_SilentFarEndNeverMatches(t *testing.T) {
	// The bot isn't speaking (far-end silent) but the caller is (near-end has real energy) --
	// this must never be flagged as echo, since there's nothing to echo.
	far := silence(320 * 10)
	near := tone(320*5, 0.3, 8000)
	farEnv := rmsEnvelope(far, echoFrameBytes)
	nearEnv := rmsEnvelope(near, echoFrameBytes)

	peak, _ := correlateEnvelopes(nearEnv, farEnv)
	if peak != 0 {
		t.Errorf("silent far-end must never correlate as an echo source, got %f", peak)
	}
}

func TestCorrelateEnvelopes_FarShorterThanNearReturnsNoMatch(t *testing.T) {
	near := rmsEnvelope(tone(320*10, 0.3, 8000), echoFrameBytes)
	far := rmsEnvelope(tone(320*3, 0.3, 8000), echoFrameBytes)
	peak, atFrame := correlateEnvelopes(near, far)
	if peak != 0 || atFrame != -1 {
		t.Errorf("far shorter than near must report no match, got peak=%f atFrame=%d", peak, atFrame)
	}
}

func TestIsLikelyAcousticEcho_NotEnoughNearFramesIsSafe(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	vad := NewRMSVAD(0.05, 50*time.Millisecond)
	cfg := DefaultConfig()
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("short-near"))
	defer stream.Close()

	stream.echoMu.Lock()
	stream.echoFarEndBuf = tone(320*20, 0.3, 8000)
	stream.echoNearEndBuf = tone(20, 0.3, 8000) // far less than one 320-sample frame
	stream.echoNearEndGen = 0
	stream.echoMu.Unlock()

	echo, score, _, nearFrames := stream.isLikelyAcousticEcho(0)
	if echo {
		t.Error("too little near-end audio must never be treated as a confirmed echo")
	}
	if nearFrames >= echoMinNearFrames {
		t.Errorf("expected nearFrames below the minimum, got %d", nearFrames)
	}
	if score != 0 {
		t.Errorf("expected score 0 on early exit, got %f", score)
	}
}

func TestIsLikelyAcousticEcho_StaleGenerationIsIgnored(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	vad := NewRMSVAD(0.05, 50*time.Millisecond)
	cfg := DefaultConfig()
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("stale-gen"))
	defer stream.Close()

	toneFrames := tone(320*5, 0.3, 8000)
	stream.echoMu.Lock()
	stream.echoFarEndBuf = toneFrames
	stream.echoNearEndBuf = attenuate(toneFrames, 0.4)
	stream.echoNearEndGen = 1 // buffer belongs to generation 1
	stream.echoMu.Unlock()

	// Asking about generation 2 (a newer turn) must not see generation 1's leftover audio.
	echo, _, _, nearFrames := stream.isLikelyAcousticEcho(2)
	if echo {
		t.Error("a stale generation's near-end audio must not confirm echo for a newer turn")
	}
	if nearFrames != 0 {
		t.Errorf("expected 0 near frames for a non-matching generation, got %d", nearFrames)
	}
}
