package orchestrator

import (
	"math"
	"sync"
	"time"
)

type RMSVAD struct {
	threshold    float64
	silenceLimit time.Duration
	isSpeaking   bool
	silenceStart time.Time

	adaptiveMode bool
	noiseFloor   float64
	alpha        float64

	consecutiveFrames int
	minConfirmed      int
	lastRMS           float64
	localMin          float64
	lastMinUpdate     time.Time
	mu                sync.Mutex
}

func NewRMSVAD(threshold float64, silenceLimit time.Duration) *RMSVAD {
	return &RMSVAD{
		threshold:    threshold,
		silenceLimit: silenceLimit,
		minConfirmed: 7,
		adaptiveMode: true,
		noiseFloor:   0.005,
		alpha:        0.05,
	}
}

func (v *RMSVAD) SetAdaptiveMode(enabled bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.adaptiveMode = enabled
}

func (v *RMSVAD) SetMinConfirmed(count int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.minConfirmed = count
}

func (v *RMSVAD) MinConfirmed() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.minConfirmed
}

func (v *RMSVAD) SetThreshold(threshold float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.threshold = threshold
}

func (v *RMSVAD) Threshold() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.threshold
}

func (v *RMSVAD) LastRMS() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastRMS
}

func (v *RMSVAD) IsSpeaking() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.isSpeaking
}

func (v *RMSVAD) Process(chunk []byte) (*VADEvent, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	rms := v.calculateRMS(chunk)
	v.lastRMS = rms
	now := time.Now()

	effectiveThreshold := v.threshold
	if v.adaptiveMode {

		// Skip adapting noise floor if it's pure digital silence (like injected echo suppression)
		if rms > 0.0001 {
			if rms < v.noiseFloor {
				v.noiseFloor = rms
				v.localMin = rms
			} else if !v.isSpeaking {
				v.noiseFloor = (1-v.alpha)*v.noiseFloor + v.alpha*rms
				v.localMin = v.noiseFloor
			} else {
				// Even while speaking, track the local minimum to adapt if noise increases
				if rms < v.localMin || v.lastMinUpdate.IsZero() {
					v.localMin = rms
					if v.lastMinUpdate.IsZero() {
						v.lastMinUpdate = now
					}
				}
				if now.Sub(v.lastMinUpdate) > 2*time.Second {
					// We've been "speaking" for 2s, but the floor has moved up.
					// This is likely noise, not continuous speech.
					v.noiseFloor = v.localMin
					v.lastMinUpdate = now
					v.localMin = rms
				}
			}
		}

		adaptiveThreshold := v.noiseFloor * 2.0
		if adaptiveThreshold > effectiveThreshold {
			effectiveThreshold = adaptiveThreshold
		}

		if effectiveThreshold > 0.3 {
			effectiveThreshold = 0.3
		}
	}

	if rms > effectiveThreshold {
		v.consecutiveFrames++
		if !v.isSpeaking {

			if v.consecutiveFrames >= v.minConfirmed {
				v.isSpeaking = true
				return &VADEvent{Type: VADSpeechStart, Timestamp: now.UnixMilli()}, nil
			}
			return nil, nil
		}
		v.silenceStart = time.Time{}
		return nil, nil
	}

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
	v.mu.Lock()
	defer v.mu.Unlock()
	v.isSpeaking = false
	v.silenceStart = time.Time{}
	v.consecutiveFrames = 0
}

func (v *RMSVAD) Clone() VADProvider {
	v.mu.Lock()
	defer v.mu.Unlock()
	return &RMSVAD{
		threshold:     v.threshold,
		silenceLimit:  v.silenceLimit,
		minConfirmed:  v.minConfirmed,
		adaptiveMode:  v.adaptiveMode,
		noiseFloor:    v.noiseFloor,
		alpha:         v.alpha,
		localMin:      v.localMin,
		lastMinUpdate: v.lastMinUpdate,
	}
}

func (v *RMSVAD) calculateRMS(chunk []byte) float64 {
	if len(chunk) == 0 {
		return 0
	}

	var sum float64

	for i := 0; i < len(chunk)-1; i += 2 {
		sample := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		f := float64(sample) / 32768.0
		sum += f * f
	}

	return math.Sqrt(sum / float64(len(chunk)/2))
}
