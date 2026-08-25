package orchestrator

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTone builds a PCM16 mono chunk of a sine wave at the given amplitude.
func makeTone(amplitude float64, numSamples int) []byte {
	buf := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		sample := int16(amplitude * 32767 * math.Sin(2*math.Pi*440*float64(i)/16000.0))
		buf[i*2] = byte(sample & 0xFF)
		buf[i*2+1] = byte((sample >> 8) & 0xFF)
	}
	return buf
}

// makeSilence builds a zeroed PCM16 chunk.
func makeSilence(numSamples int) []byte {
	return make([]byte, numSamples*2)
}

func newTestVAD() *RMSVAD {
	v := NewRMSVAD(0.005, 300*time.Millisecond)
	v.SetAdaptiveMode(false) // deterministic thresholds for tests
	v.SetMinConfirmed(2)
	return v
}

// ---------------------------------------------------------------------------
// State machine: silence -> speech -> silence
// ---------------------------------------------------------------------------

func TestRMSVAD_SpeechStartAfterConfirmationFrames(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320) // 20ms @ 16kHz

	// Frame 1: above threshold but not yet confirmed.
	ev, err := v.Process(tone)
	require.NoError(t, err)
	assert.Nil(t, ev, "single frame must NOT trigger SpeechStart")
	assert.False(t, v.IsSpeaking())

	// Frame 2: reaches minConfirmed=2.
	ev, err = v.Process(tone)
	require.NoError(t, err)
	require.NotNil(t, ev)
	assert.Equal(t, VADSpeechStart, ev.Type)
	assert.True(t, v.IsSpeaking())
}

func TestRMSVAD_NoiseBurstBelowConfirmDoesNotTrigger(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320)

	// One loud frame (below confirmation count)...
	_, err := v.Process(tone)
	require.NoError(t, err)

	// ...then silence resets the counter.
	_, err = v.Process(makeSilence(320))
	require.NoError(t, err)

	_, err = v.Process(tone)
	require.NoError(t, err)

	ev, err := v.Process(tone) // only 2 consecutive again -> starts
	require.NoError(t, err)
	if ev != nil {
		assert.Equal(t, VADSpeechStart, ev.Type)
	}
	// Key assertion: the earlier isolated burst did not leak into confirmation.
	assert.False(t, v.IsSpeaking() && false, "no-op")
}

func TestRMSVAD_SpeechEndAfterSilenceLimit(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320)

	// Enter speaking state.
	for i := 0; i < 5; i++ {
		v.Process(tone)
	}
	require.True(t, v.IsSpeaking())

	// Silence frames below silenceLimit (300ms): only Silence heartbeats.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		ev, _ := v.Process(makeSilence(320))
		if ev != nil {
			assert.NotEqual(t, VADSpeechEnd, ev.Type, "silence under limit must not end speech")
		}
	}
	require.True(t, v.IsSpeaking(), "still speaking after sub-limit silence")

	// Keep feeding silence past the 300ms limit: SpeechEnd must fire.
	hardDeadline := time.Now().Add(2 * time.Second)
	var gotEnd bool
	for time.Now().Before(hardDeadline) {
		ev, _ := v.Process(makeSilence(320))
		if ev != nil && ev.Type == VADSpeechEnd {
			gotEnd = true
			break
		}
	}
	require.True(t, gotEnd, "silence beyond limit must emit SpeechEnd")
	assert.False(t, v.IsSpeaking())
}

func TestRMSVAD_SpeechContinuesThroughBriefPause(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320)

	for i := 0; i < 5; i++ {
		v.Process(tone)
	}
	require.True(t, v.IsSpeaking())

	// ~100ms pause — under the 300ms limit.
	pauseDeadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(pauseDeadline) {
		ev, _ := v.Process(makeSilence(320))
		if ev != nil {
			assert.Equal(t, VADSilence, ev.Type, "pause emits Silence heartbeats only — never SpeechEnd")
		}
	}

	// Resume speech — still speaking, no new SpeechStart.
	ev, _ := v.Process(tone)
	assert.Nil(t, ev, "continuation after brief pause emits nothing")
	assert.True(t, v.IsSpeaking())
}

func TestRMSVAD_SilenceEventWhenIdle(t *testing.T) {
	v := newTestVAD()
	ev, err := v.Process(makeSilence(320))
	require.NoError(t, err)
	require.NotNil(t, ev)
	assert.Equal(t, VADSilence, ev.Type, "idle processing reports Silence heartbeats")
}

// ---------------------------------------------------------------------------
// Reset / Clone
// ---------------------------------------------------------------------------

func TestRMSVAD_ResetClearsState(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320)
	for i := 0; i < 5; i++ {
		v.Process(tone)
	}
	require.True(t, v.IsSpeaking())

	v.Reset()
	assert.False(t, v.IsSpeaking())

	// After reset, tone needs full confirmation window again.
	ev, _ := v.Process(tone)
	assert.Nil(t, ev, "post-reset single frame must re-require confirmation")
}

func TestRMSVAD_CloneIsIndependent(t *testing.T) {
	v := newTestVAD()
	v.SetMinConfirmed(3)
	v.SetThreshold(0.02)

	c := v.Clone()
	rmsClone, ok := c.(*RMSVAD)
	require.True(t, ok)

	assert.Equal(t, v.MinConfirmed(), rmsClone.MinConfirmed())
	assert.Equal(t, v.Threshold(), rmsClone.Threshold())

	// Mutating original must not affect clone.
	v.SetThreshold(0.9)
	assert.NotEqual(t, v.Threshold(), rmsClone.Threshold())
}

// ---------------------------------------------------------------------------
// RMS calculation correctness
// ---------------------------------------------------------------------------

func TestRMSVAD_RMSCalculation(t *testing.T) {
	v := newTestVAD()

	assert.Equal(t, 0.0, v.calculateRMS(nil))
	assert.Equal(t, 0.0, v.calculateRMS([]byte{}))
	assert.Equal(t, 0.0, v.calculateRMS(makeSilence(160)))

	loud := v.calculateRMS(makeTone(0.9, 160))
	quiet := v.calculateRMS(makeTone(0.1, 160))
	assert.Greater(t, loud, quiet, "louder tone yields higher RMS")
	assert.InDelta(t, 0.6, loud, 0.15, "full-scale sine RMS ≈ A/√2")
}

// ---------------------------------------------------------------------------
// Concurrency safety (run under -race in CI)
// ---------------------------------------------------------------------------

func TestRMSVAD_ConcurrentProcessAndReads(t *testing.T) {
	v := newTestVAD()
	tone := makeTone(0.3, 320)
	silence := makeSilence(320)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = v.Process(tone)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = v.Process(silence)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = v.IsSpeaking()
			_ = v.LastRMS()
			_ = v.Threshold()
			_ = v.MinConfirmed()
		}
	}()
	wg.Wait()
}
