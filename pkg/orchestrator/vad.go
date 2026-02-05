package orchestrator

import (
	"math"
	"time"
)

// RMSVAD is a simple Root Mean Square based Voice Activity Detector
// It's useful as a lightweight, no-dependency default.
type RMSVAD struct {
	threshold    float64
	silenceLimit time.Duration
	isSpeaking   bool
	silenceStart time.Time

	// Hysteresis and confirmed speech detection
	consecutiveFrames int
	minConfirmed      int
}

// NewRMSVAD creates a new RMS-based VAD
func NewRMSVAD(threshold float64, silenceLimit time.Duration) *RMSVAD {
	return &RMSVAD{
		threshold:    threshold,
		silenceLimit: silenceLimit,
		minConfirmed: 5, // Increased to ~100-150ms of continuous sound to filter echo-onset
	}
}

// SetMinConfirmed sets the number of consecutive frames needed to confirm speech start
func (v *RMSVAD) SetMinConfirmed(count int) {
	v.minConfirmed = count
}

// SetThreshold updates the RMS threshold
func (v *RMSVAD) SetThreshold(threshold float64) {
	v.threshold = threshold
}

// Threshold returns the current RMS threshold
func (v *RMSVAD) Threshold() float64 {
	return v.threshold
}

func (v *RMSVAD) Process(chunk []byte) (*VADEvent, error) {
	rms := v.calculateRMS(chunk)
	now := time.Now()

	if rms > v.threshold {
		v.consecutiveFrames++
		if !v.isSpeaking {
			// Require a sequence of frames above threshold to filter out spikes and echo-onset pops
			if v.consecutiveFrames >= v.minConfirmed {
				v.isSpeaking = true
				return &VADEvent{Type: VADSpeechStart, Timestamp: now.UnixMilli()}, nil
			}
			return nil, nil // Still confirming
		}
		v.silenceStart = time.Time{} // Reset silence timer
		return nil, nil
	}

	// Below threshold
	v.consecutiveFrames = 0

	if v.isSpeaking {
		if v.silenceStart.IsZero() {
			v.silenceStart = now
		}

		if now.Sub(v.silenceStart) >= v.silenceLimit {
			v.isSpeaking = false
			v.silenceStart = time.Time{}
			return &VADEvent{Type: VADSpeechEnd, Timestamp: now.UnixMilli()}, nil
		}
	}

	return &VADEvent{Type: VADSilence, Timestamp: now.UnixMilli()}, nil
}

func (v *RMSVAD) Name() string {
	return "rms_vad"
}

func (v *RMSVAD) Reset() {
	v.isSpeaking = false
	v.silenceStart = time.Time{}
	v.consecutiveFrames = 0
}

func (v *RMSVAD) Clone() VADProvider {
	return &RMSVAD{
		threshold:    v.threshold,
		silenceLimit: v.silenceLimit,
		minConfirmed: v.minConfirmed,
	}
}

func (v *RMSVAD) calculateRMS(chunk []byte) float64 {
	if len(chunk) == 0 {
		return 0
	}

	var sum float64
	// Assuming 16-bit PCM (2 bytes per sample)
	for i := 0; i < len(chunk)-1; i += 2 {
		sample := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		f := float64(sample) / 32768.0
		sum += f * f
	}

	return math.Sqrt(sum / float64(len(chunk)/2))
}
