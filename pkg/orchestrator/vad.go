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

	// Adaptive noise floor tracking
	adaptiveMode bool
	noiseFloor   float64
	alpha        float64 // Attack rate for noise floor tracking

	// Hysteresis and confirmed speech detection
	consecutiveFrames int
	minConfirmed      int
	lastRMS           float64
}

// NewRMSVAD creates a new RMS-based VAD
func NewRMSVAD(threshold float64, silenceLimit time.Duration) *RMSVAD {
	return &RMSVAD{
		threshold:    threshold,
		silenceLimit: silenceLimit,
		minConfirmed: 7, // Require ~70-100ms of continuous sound to trigger snappier barge-in
		adaptiveMode: true,
		noiseFloor:   0.005, // Initial low noise floor
		alpha:        0.05,  // Slow adaptation
	}
}

// SetAdaptiveMode enables or disables noise floor tracking
func (v *RMSVAD) SetAdaptiveMode(enabled bool) {
	v.adaptiveMode = enabled
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

// LastRMS returns the RMS of the last processed chunk
func (v *RMSVAD) LastRMS() float64 {
	return v.lastRMS
}

// IsSpeaking returns true if speech is currently detected
func (v *RMSVAD) IsSpeaking() bool {
	return v.isSpeaking
}

func (v *RMSVAD) Process(chunk []byte) (*VADEvent, error) {
	rms := v.calculateRMS(chunk)
	v.lastRMS = rms
	now := time.Now()

	// Adaptive threshold adjustment
	effectiveThreshold := v.threshold
	if v.adaptiveMode {
		// If current sound is lower than noise floor, it's the new noise floor
		if rms < v.noiseFloor {
			v.noiseFloor = rms
		} else if !v.isSpeaking && rms < v.threshold*2 {
			// Slowly adapt noise floor upwards if we are not speaking and sound is relatively low
			v.noiseFloor = (1-v.alpha)*v.noiseFloor + v.alpha*rms
		}

		// Ensure threshold is always at least 2.0x the background noise floor.
		// A 2.0x multiplier is more sensitive for barge-in while still filtering noise.
		adaptiveThreshold := v.noiseFloor * 2.0
		if adaptiveThreshold > effectiveThreshold {
			effectiveThreshold = adaptiveThreshold
		}

		// Cap the effective threshold to 0.3 so we don't become "deaf" in noisy environments
		if effectiveThreshold > 0.3 {
			effectiveThreshold = 0.3
		}
	}

	if rms > effectiveThreshold {
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
