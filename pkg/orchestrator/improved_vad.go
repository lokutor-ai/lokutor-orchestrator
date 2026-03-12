package orchestrator

import (
	"math"
	"sync"
	"time"
)

// ImprovedRMSVAD is optimized for extreme environments like conferences.
// It uses EMA, Zero Crossing Rate (ZCR), and Peak tracking to differentiate speech from noise.
type ImprovedRMSVAD struct {
	mu           sync.Mutex
	threshold    float64
	silenceLimit time.Duration
	isSpeaking   bool
	silenceStart time.Time

	noiseFloor float64
	emaRMS     float64
	alphaEMA   float64

	// Zero Crossing Rate tracking (ZCR)
	emaZCR   float64
	alphaZCR float64
	voiceZCR float64

	// Peak tracking for PAPR filter
	peakRMS   float64
	alphaPeak float64

	// Temporal Persistence
	energyWindow []float64
	windowIdx    int

	// Advanced noise tracking
	lastMinRMS   float64
	minTrackerAt time.Time
	isWarmup     bool
	warmupCount  int

	consecutiveFrames int
	minConfirmed      int
	lastRMS           float64

	adaptiveMode bool
	sampleRate   int
}

func NewImprovedRMSVAD(threshold float64, silenceLimit time.Duration, sampleRate int) *ImprovedRMSVAD {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	return &ImprovedRMSVAD{
		threshold:    threshold,
		silenceLimit: silenceLimit,
		minConfirmed: 6, // Increased to reduce false starts
		noiseFloor:   threshold,
		alphaEMA:     0.25,
		alphaZCR:     0.1,
		alphaPeak:    0.05,
		adaptiveMode: true,
		lastMinRMS:   1.0,
		minTrackerAt: time.Now(),
		isWarmup:     true,
		voiceZCR:     0.02,
		energyWindow: make([]float64, 5),
		sampleRate:   sampleRate,
	}
}

func (v *ImprovedRMSVAD) SetThreshold(t float64) { v.mu.Lock(); defer v.mu.Unlock(); v.threshold = t }
func (v *ImprovedRMSVAD) SetAdaptiveMode(b bool) { v.mu.Lock(); defer v.mu.Unlock(); v.adaptiveMode = b }
func (v *ImprovedRMSVAD) SetMinConfirmed(n int)  { v.mu.Lock(); defer v.mu.Unlock(); v.minConfirmed = n }
func (v *ImprovedRMSVAD) Threshold() float64      { v.mu.Lock(); defer v.mu.Unlock(); return v.threshold }
func (v *ImprovedRMSVAD) MinConfirmed() int      { v.mu.Lock(); defer v.mu.Unlock(); return v.minConfirmed }
func (v *ImprovedRMSVAD) IsSpeaking() bool       { v.mu.Lock(); defer v.mu.Unlock(); return v.isSpeaking }
func (v *ImprovedRMSVAD) LastRMS() float64       { v.mu.Lock(); defer v.mu.Unlock(); return v.lastRMS }
func (v *ImprovedRMSVAD) Name() string           { return "improved_rms_vad" }

func (v *ImprovedRMSVAD) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.isSpeaking = false
	v.silenceStart = time.Time{}
	v.consecutiveFrames = 0
}

func (v *ImprovedRMSVAD) Clone() VADProvider {
	v.mu.Lock()
	defer v.mu.Unlock()
	return &ImprovedRMSVAD{
		threshold:    v.threshold,
		silenceLimit: v.silenceLimit,
		minConfirmed: v.minConfirmed,
		noiseFloor:   v.noiseFloor,
		alphaEMA:     v.alphaEMA,
		alphaZCR:     v.alphaZCR,
		adaptiveMode: v.adaptiveMode,
		voiceZCR:     v.voiceZCR,
		energyWindow: make([]float64, 5),
		sampleRate:   v.sampleRate,
	}
}

func (v *ImprovedRMSVAD) analyze(chunk []byte) (float64, float64, float64) {
	if len(chunk) == 0 {
		return 0, 0, 0
	}
	var sum float64
	var maxAbs float64
	crossings := 0
	lastSample := float64(0)

	validSamples := 0
	for i := 0; i < len(chunk)-1; i += 2 {
		sampleInt := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		f := float64(sampleInt) / 32768.0

		absF := math.Abs(f)
		if absF > maxAbs {
			maxAbs = absF
		}

		sum += f * f
		if (f > 0 && lastSample <= 0) || (f < 0 && lastSample >= 0) {
			crossings++
		}
		lastSample = f
		validSamples++
	}

	rms := math.Sqrt(sum / float64(validSamples))
	zcr := float64(crossings) / float64(validSamples)

	return rms, zcr, maxAbs
}

func (v *ImprovedRMSVAD) Process(chunk []byte) (*VADEvent, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	rms, zcr, peak := v.analyze(chunk)
	v.lastRMS = rms
	now := time.Now()

	// Update Energy Persistence Window
	v.energyWindow[v.windowIdx] = rms
	v.windowIdx = (v.windowIdx + 1) % len(v.energyWindow)

	sustainedEnergy := 0.0
	for _, val := range v.energyWindow {
		sustainedEnergy += val
	}
	sustainedEnergy /= float64(len(v.energyWindow))

	// Update EMAs
	v.emaRMS = (1-v.alphaEMA)*v.emaRMS + v.alphaEMA*rms
	v.emaZCR = (1-v.alphaZCR)*v.emaZCR + v.alphaZCR*zcr

	if peak > v.peakRMS {
		v.peakRMS = peak
	} else {
		v.peakRMS = (1-v.alphaPeak)*v.peakRMS + v.alphaPeak*peak
	}

	// 1. Noise Floor Adaptation
	if v.isWarmup {
		v.warmupCount++
		v.noiseFloor = (v.noiseFloor * 0.9) + (rms * 0.1)
		if v.warmupCount > 20 {
			v.isWarmup = false
		}
	} else {
		if rms < v.lastMinRMS {
			v.lastMinRMS = rms
		}
		if now.Sub(v.minTrackerAt) > 1000*time.Millisecond {
			if v.lastMinRMS > 0.0001 {
				v.noiseFloor = (v.noiseFloor * 0.7) + (v.lastMinRMS * 0.3)
			}
			v.lastMinRMS = 1.0
			v.minTrackerAt = now
		}
	}

	// 2. Dynamic Thresholding - Using a more balanced margin
	margin := 2.2
	if v.noiseFloor > 0.01 {
		margin = 3.0 // A bit more strict if it's noisy
	}

	effectiveThreshold := v.threshold
	adaptiveThreshold := v.noiseFloor * margin
	if adaptiveThreshold > effectiveThreshold {
		effectiveThreshold = adaptiveThreshold
	}

	// Hard caps
	if effectiveThreshold < 0.005 {
		effectiveThreshold = 0.005
	}
	if effectiveThreshold > 0.35 {
		effectiveThreshold = 0.35
	}

	// 3. Filters
	crestFactor := 0.0
	if rms > 0.001 {
		crestFactor = peak / rms
	}
	isImpulsive := crestFactor > 7.0

	penalty := 1.0

	// ZCR Penalty - Adjusted for potentially higher sample rates (original was 16kHz)
	zcrLimitLow := 0.001
	zcrLimitHigh := 0.15
	if v.sampleRate > 16000 {
		// Loosen ZCR slightly for higher sample rates
		zcrLimitHigh = 0.25 
	}

	if v.emaZCR < zcrLimitLow || v.emaZCR > zcrLimitHigh {
		penalty *= 3.0
	}

	if isImpulsive {
		penalty *= 2.0
	}

	// Require Energy Persistence to start
	if !v.isSpeaking && sustainedEnergy < (effectiveThreshold*0.5) {
		penalty *= 2.0
	}

	targetThreshold := effectiveThreshold * penalty

	// 5. Detection Logic
	if v.emaRMS > targetThreshold {
		v.consecutiveFrames++
		if !v.isSpeaking {
			if v.consecutiveFrames == 1 {
				// Instant signal for muting
				return &VADEvent{Type: VADSpeechPotential, Timestamp: now.UnixMilli()}, nil
			}
			if v.consecutiveFrames >= v.minConfirmed {
				v.isSpeaking = true
				v.silenceStart = time.Time{}
				return &VADEvent{Type: VADSpeechStart, Timestamp: now.UnixMilli()}, nil
			}
		} else {
			v.silenceStart = time.Time{}
		}
		return nil, nil
	}

	// Silence detection
	v.consecutiveFrames = 0
	if v.isSpeaking {
		if v.silenceStart.IsZero() {
			v.silenceStart = now
		}

		limit := v.silenceLimit
		if v.emaRMS < effectiveThreshold {
			limit = 150 * time.Millisecond // Fast drop for preemptive trigger
		}

		if now.Sub(v.silenceStart) >= limit {
			v.isSpeaking = false
			v.silenceStart = time.Time{}
			return &VADEvent{Type: VADSpeechEnd, Timestamp: now.UnixMilli()}, nil
		}
	}

	return nil, nil
}
