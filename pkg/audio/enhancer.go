package audio

import (
	"encoding/binary"
	"math"
	"sync"
)

// Processor applies acoustic enhancements to TTS output
// This is where you get the "warmth" and quality boost without changing the model
type Processor struct {
	config    Config
	mu        sync.Mutex
	history   []float64 // for adaptive processing
}

// Config holds audio processing parameters
type Config struct {
	// Loudness normalization
	TargetLUFS  float64 // -14 to -24, default -16
	TruePeak    float64 // dB, default -1.0

	// Room simulation
	ReverbRoomSize float64 // 0.0 - 1.0, default 0.2
	ReverbMix      float64 // 0.0 - 1.0, default 0.15 (15% wet)

	// EQ shaping
	LowShelfHz    float64  // Hz, default 200
	LowShelfGain  float64  // dB, default 2.0
	HighShelfHz   float64  // Hz, default 4000
	HighShelfGain float64  // dB, default -1.0
	PresenceGain  float64  // dB at 2-4kHz, default 1.5

	// Harmonic enhancement (warmth)
	HarmonicMix   float64 // 0.0 - 1.0, default 0.15
	HarmonicOrder int     // 2-4, default 2

	// Output device compensation
	OutputDevice DeviceType

	// Dynamic processing
	CompressThreshold float64 // dB, default -20
	CompressRatio     float64 // 2-10, default 4
	AttackMs          float64 // ms, default 10
	ReleaseMs         float64 // ms, default 100
}

// DeviceType represents output device characteristics
type DeviceType int

const (
	DeviceSpeaker DeviceType = iota
	DeviceHeadphone
	DeviceEarbud
	DeviceCar
	DeviceUnknown
)

func DefaultConfig() Config {
	return Config{
		TargetLUFS:       -16,
		TruePeak:         -1.0,
		ReverbRoomSize:   0.2,
		ReverbMix:        0.15,
		LowShelfHz:       200,
		LowShelfGain:     2.0,
		HighShelfHz:      4000,
		HighShelfGain:    -1.0,
		PresenceGain:     1.5,
		HarmonicMix:      0.15,
		HarmonicOrder:    2,
		OutputDevice:     DeviceSpeaker,
		CompressThreshold: -20,
		CompressRatio:    4,
		AttackMs:         10,
		ReleaseMs:        100,
	}
}

func NewProcessor(cfg Config) *Processor {
	return &Processor{
		config:  cfg,
		history: make([]float64, 1024),
	}
}

// Process applies all enhancements to raw PCM audio
// Input: 16-bit signed PCM, mono or stereo
// Output: Enhanced 16-bit PCM
func (p *Processor) Process(samples []byte, sampleRate int, channels int) []byte {
	// Convert to float64
	floatSamples := p.bytesToFloat(samples, channels)

	// Apply processing chain
	floatSamples = p.applyEQ(floatSamples, sampleRate)
	floatSamples = p.applyCompression(floatSamples)
	floatSamples = p.applyHarmonicEnhancement(floatSamples)
	floatSamples = p.applyReverb(floatSamples, sampleRate)
	floatSamples = p.applyLoudnessNormalization(floatSamples)

	// Convert back to bytes
	return p.floatToBytes(floatSamples, channels)
}

// ProcessStereo processes stereo audio with separate L/R processing
func (p *Processor) ProcessStereo(left, right []byte, sampleRate int) ([]byte, []byte) {
	leftFloat := p.bytesToFloat(left, 1)
	rightFloat := p.bytesToFloat(right, 1)

	// Process each channel
	leftFloat = p.applyEQ(leftFloat, sampleRate)
	leftFloat = p.applyCompression(leftFloat)
	leftFloat = p.applyHarmonicEnhancement(leftFloat)
	leftFloat = p.applyReverb(leftFloat, sampleRate)
	leftFloat = p.applyLoudnessNormalization(leftFloat)

	rightFloat = p.applyEQ(rightFloat, sampleRate)
	rightFloat = p.applyCompression(rightFloat)
	rightFloat = p.applyHarmonicEnhancement(rightFloat)
	rightFloat = p.applyReverb(rightFloat, sampleRate)
	rightFloat = p.applyLoudnessNormalization(rightFloat)

	return p.floatToBytes(leftFloat, 1), p.floatToBytes(rightFloat, 1)
}

func (p *Processor) bytesToFloat(data []byte, channels int) []float64 {
	samples := make([]float64, len(data)/2/channels)
	for i := range samples {
		sample := int16(binary.LittleEndian.Uint16(data[i*2*channels:(i+1)*2*channels]))
		samples[i] = float64(sample) / 32768.0
	}
	return samples
}

func (p *Processor) floatToBytes(samples []float64, channels int) []byte {
	data := make([]byte, len(samples)*2*channels)
	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		}
		if s < -1.0 {
			s = -1.0
		}
		sample := int16(s * 32767)
		binary.LittleEndian.PutUint16(data[i*2*channels:(i+1)*2*channels], uint16(sample))
	}
	return data
}

// EQ applies parametric EQ with shelving filters
func (p *Processor) applyEQ(samples []float64, sampleRate int) []float64 {
	// Simple IIR approximation using exponential smoothing
	lowAlpha := math.Exp(-2 * math.Pi * p.config.LowShelfHz / float64(sampleRate))
	highAlpha := math.Exp(-2 * math.Pi * p.config.HighShelfHz / float64(sampleRate))

	lowGain := math.Pow(10, p.config.LowShelfGain/20)
	highGain := math.Pow(10, p.config.HighShelfGain/20)
	presenceGain := math.Pow(10, p.config.PresenceGain/20)

	lowOut := 0.0
	highOut := 0.0
	presenceOut := 0.0

	result := make([]float64, len(samples))
	for i, s := range samples {
		// Low shelf
		lowOut = lowAlpha*lowOut + (1-lowAlpha)*s*lowGain
		// High shelf
		highOut = highAlpha*highOut + (1-highAlpha)*s*highGain
		// Presence (approx at 3kHz)
		presAlpha := math.Exp(-2 * math.Pi * 3000 / float64(sampleRate))
		presenceOut = presAlpha*presenceOut + (1-presAlpha)*s*presenceGain

		// Mix: dry + low + high + presence
		result[i] = s*0.5 + lowOut*0.2 + highOut*0.2 + presenceOut*0.1

		// Soft clip
		result[i] = softClip(result[i])
	}

	return result
}

// Compression applies dynamic range compression
func (p *Processor) applyCompression(samples []float64) []float64 {
	threshold := math.Pow(10, p.config.CompressThreshold/20)
	ratio := p.config.CompressRatio
	attack := math.Exp(-1 / (p.config.AttackMs * 44100 / 1000))
	release := math.Exp(-1 / (p.config.ReleaseMs * 44100 / 1000))

	gain := 1.0
	result := make([]float64, len(samples))

	for i, s := range samples {
		// Envelope
		absS := math.Abs(s)
		if absS > threshold {
			// Above threshold - attack
			gain = attack*gain + (1-attack)*threshold/absS
		} else {
			// Below threshold - release
			gain = release*gain + (1-release)*1.0
		}

		// Apply gain reduction
		reduced := s * gain

		// Hard knee compression
		if absS > threshold {
			excess := absS - threshold
			sign := s / absS
			compressed := threshold + excess/ratio
			reduced = sign * compressed
		}

		result[i] = reduced
	}

	return result
}

// HarmonicEnhancement adds even harmonics for warmth
func (p *Processor) applyHarmonicEnhancement(samples []float64) []float64 {
	if p.config.HarmonicMix == 0 {
		return samples
	}

	result := make([]float64, len(samples))
	mix := p.config.HarmonicMix

	for i, s := range samples {
		// Add 2nd harmonic (octave up, much quieter)
		h2 := math.Abs(s) * math.Sin(math.Pi*s) * 0.3
		// Add 3rd harmonic for body
		h3 := math.Abs(s) * math.Sin(2*math.Pi*s) * 0.15

		harmonics := (h2 + h3) * mix
		result[i] = s + harmonics

		// Soft clip
		result[i] = softClip(result[i])
	}

	return result
}

// Reverb applies simple room simulation using comb filters
func (p *Processor) applyReverb(samples []float64, sampleRate int) []float64 {
	if p.config.ReverbMix == 0 || p.config.ReverbRoomSize == 0 {
		return samples
	}

	// Simple comb filter-based reverb
	delayMs := 20 + p.config.ReverbRoomSize*40 // 20-60ms
	delaySamples := int(float64(delayMs) * float64(sampleRate) / 1000)

	decay := 0.5 - p.config.ReverbRoomSize*0.3 // 0.2-0.5

	result := make([]float64, len(samples))
	delayLine := make([]float64, delaySamples)
	writeIdx := 0

	for i, s := range samples {
		// Read from delay line
		readIdx := (writeIdx - delaySamples + len(delayLine)) % len(delayLine)
		delayed := delayLine[readIdx]

		// Write to delay line with decay
		delayLine[writeIdx] = s + delayed*decay
		writeIdx = (writeIdx + 1) % len(delayLine)

		// Mix wet and dry
		result[i] = s*(1-p.config.ReverbMix) + delayed*p.config.ReverbMix

		// Soft clip
		result[i] = softClip(result[i])
	}

	return result
}

// LoudnessNormalization applies LUFS-style loudness normalization
func (p *Processor) applyLoudnessNormalization(samples []float64) []float64 {
	// Calculate RMS
	var sumSq float64
	for _, s := range samples {
		sumSq += s * s
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))

	if rms < 0.001 {
		return samples
	}

	// Convert to LUFS (approximation)
	currentLUFS := -0.691 + 10*math.Log10(rms)
	targetLUFS := p.config.TargetLUFS

	gain := math.Pow(10, (targetLUFS-currentLUFS)/20)

	// Apply gain with limiting
	result := make([]float64, len(samples))
	for i, s := range samples {
		g := s * gain
		// True peak limiting
		if g > 0.99 {
			g = 0.99 + (g-0.99)*0.1
		} else if g < -0.99 {
			g = -0.99 + (g+0.99)*0.1
		}
		result[i] = g
	}

	return result
}

// softClip applies soft clipping for natural limiting
func softClip(x float64) float64 {
	if x > 1.0 {
		return 1.0 - math.Exp(-(x-1.0))
	}
	if x < -1.0 {
		return -1.0 + math.Exp(-(-x-1.0))
	}
	return x
}

// AdaptiveProcessor adjusts processing based on audio content
type AdaptiveProcessor struct {
	baseConfig Config
	mu         sync.RWMutex
}

func NewAdaptiveProcessor(cfg Config) *AdaptiveProcessor {
	return &AdaptiveProcessor{baseConfig: cfg}
}

// SetDevice adjusts config for specific output device
func (ap *AdaptiveProcessor) SetDevice(device DeviceType) {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	ap.baseConfig.OutputDevice = device
}

// AnalyzeAndProcess performs content-aware processing
func (ap *AdaptiveProcessor) AnalyzeAndProcess(samples []byte, sampleRate, channels int) []byte {
	ap.mu.Lock()
	cfg := ap.baseConfig
	ap.mu.Unlock()

	// Analyze content
	floatSamples := ap.bytesToFloatSimple(samples, channels)

	// Detect speech vs silence
	speechRatio := ap.detectSpeechRatio(floatSamples)

	// Adjust processing based on content
	if speechRatio < 0.1 {
		// Mostly silence - reduce all processing
		cfg.ReverbMix = 0
		cfg.HarmonicMix = 0
	}

	proc := NewProcessor(cfg)
	return proc.Process(samples, sampleRate, channels)
}

func (ap *AdaptiveProcessor) detectSpeechRatio(samples []float64) float64 {
	// Simple energy-based speech detection
	threshold := 0.01
	speechSamples := 0

	for _, s := range samples {
		if math.Abs(s) > threshold {
			speechSamples++
		}
	}

	return float64(speechSamples) / float64(len(samples))
}

func (ap *AdaptiveProcessor) bytesToFloatSimple(data []byte, channels int) []float64 {
	samples := make([]float64, len(data)/2/channels)
	for i := range samples {
		sample := int16(binary.LittleEndian.Uint16(data[i*2*channels:(i+1)*2*channels]))
		samples[i] = float64(sample) / 32768.0
	}
	return samples
}

// SetDevice adjusts config for specific output device
func (p *Processor) SetDevice(device DeviceType) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.config.OutputDevice = device

	switch device {
	case DeviceHeadphone:
		// More bass, less reverb (closed space)
		p.config.LowShelfGain = 3.0
		p.config.ReverbMix = 0.1
		p.config.ReverbRoomSize = 0.1

	case DeviceEarbud:
		// Compensate for bass loss
		p.config.LowShelfGain = 4.0
		p.config.HighShelfGain = -2.0 // Reduce harshness
		p.config.PresenceGain = 0.5

	case DeviceCar:
		// Compensate for car acoustic environment
		p.config.LowShelfGain = -2.0 // Less bass (car boominess)
		p.config.HighShelfGain = 1.0 // More clarity
		p.config.PresenceGain = 2.0 // Improve speech clarity
		p.config.ReverbRoomSize = 0.3 // Simulate cabin
		p.config.ReverbMix = 0.2
	}
}