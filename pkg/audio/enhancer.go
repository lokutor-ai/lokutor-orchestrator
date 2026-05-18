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

	// Pitch modification (conversational intonation)
	PitchShift float64 // semitones, 0 = disabled, -3 to +3 typical range
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
	// Skip all processing when no effects are active
	if p.config.LowShelfGain == 0 && p.config.HighShelfGain == 0 &&
		p.config.PresenceGain == 0 && p.config.HarmonicMix == 0 &&
		p.config.ReverbMix == 0 && p.config.CompressRatio <= 1 &&
		p.config.TargetLUFS == 0 && p.config.PitchShift == 0 {
		return samples
	}

	// Convert to float64
	floatSamples := p.bytesToFloat(samples, channels)

	// Apply processing chain
	floatSamples = p.applyPitchShift(floatSamples, sampleRate)
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
	leftFloat = p.applyPitchShift(leftFloat, sampleRate)
	leftFloat = p.applyEQ(leftFloat, sampleRate)
	leftFloat = p.applyCompression(leftFloat)
	leftFloat = p.applyHarmonicEnhancement(leftFloat)
	leftFloat = p.applyReverb(leftFloat, sampleRate)
	leftFloat = p.applyLoudnessNormalization(leftFloat)

	rightFloat = p.applyPitchShift(rightFloat, sampleRate)
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

func (p *Processor) applyPitchShift(samples []float64, sampleRate int) []float64 {
	if p.config.PitchShift == 0 {
		return samples
	}
	ratio := math.Pow(2, p.config.PitchShift/12)
	return pitchShiftPSOLA(samples, ratio, sampleRate)
}

// pitchShiftPSOLA shifts pitch using Pitch-Synchronous Overlap-Add.
// ratio > 1 raises pitch, ratio < 1 lowers pitch.
func pitchShiftPSOLA(input []float64, ratio float64, sampleRate int) []float64 {
	if len(input) < 256 {
		return simpleResampleShift(input, ratio)
	}

	// Detect pitch periods
	minPeriod := sampleRate / 600 // ~73 at 44.1kHz (600Hz max)
	maxPeriod := sampleRate / 50  // ~882 at 44.1kHz (50Hz min)
	periods := detectPitchPeriods(input, minPeriod, maxPeriod, sampleRate)

	if len(periods) < 4 {
		return simpleResampleShift(input, ratio)
	}

	// Build pitch marks (cumulative sum of periods)
	marks := make([]int, 0, len(periods)+1)
	pos := periods[0] / 2
	if pos < 0 {
		pos = 0
	}
	marks = append(marks, pos)
	for _, p := range periods {
		pos += p
		if pos >= len(input) {
			break
		}
		marks = append(marks, pos)
	}
	if len(marks) < 3 {
		return simpleResampleShift(input, ratio)
	}

	outputLen := len(input)
	output := make([]float64, outputLen)
	overlap := make([]float64, outputLen) // track normalization

	synthPos := 0.0
	for i := 0; i < len(marks)-1; i++ {
		center := marks[i]
		origPeriod := marks[i+1] - marks[i]
		if origPeriod < 2 {
			continue
		}

		// Analysis window: 2 periods wide, centered on pitch mark
		halfWin := origPeriod
		winStart := center - halfWin
		if winStart < 0 {
			winStart = 0
		}
		winEnd := center + halfWin
		if winEnd > len(input) {
			winEnd = len(input)
		}
		winLen := winEnd - winStart

		// Place at synthesis position and overlap-add
		outStart := int(synthPos) - halfWin
		if outStart < 0 {
			outStart = 0
		}
		outEnd := outStart + winLen
		if outEnd > outputLen {
			outEnd = outputLen
		}
		// Adjust if we shifted relative to center
		offset := int(synthPos) - halfWin - outStart
		for j := outStart; j < outEnd; j++ {
			srcIdx := winStart + (j - outStart) + offset
			if srcIdx < 0 || srcIdx >= len(input) {
				continue
			}
			// Hanning window
			rel := float64(j-outStart) / float64(winLen)
			win := 0.5 * (1 - math.Cos(2*math.Pi*rel))
			output[j] += input[srcIdx] * win
			overlap[j] += win
		}

		synthPos += float64(origPeriod) / ratio
		if int(synthPos) >= len(input) {
			break
		}
	}

	// Normalize by overlap count
	for i := range output {
		if overlap[i] > 0.001 {
			output[i] /= overlap[i]
		}
		// Soft clip
		output[i] = softClip(output[i])
	}

	return output
}

// detectPitchPeriods finds approximate pitch periods using autocorrelation.
func detectPitchPeriods(input []float64, minPeriod, maxPeriod, sampleRate int) []int {
	var periods []int
	hop := maxPeriod / 4
	if hop < 1 {
		hop = 1
	}

	for start := 0; start < len(input)-maxPeriod; start += hop {
		bestLag := minPeriod
		bestCorr := 0.0

		for lag := minPeriod; lag <= maxPeriod && start+lag+maxPeriod < len(input); lag++ {
			var corr float64
			var energy float64
			for i := 0; i < maxPeriod && start+i < len(input) && start+i+lag < len(input); i++ {
				corr += input[start+i] * input[start+i+lag]
				energy += input[start+i] * input[start+i]
			}
			if energy > 1e-10 {
				corr /= energy
			}
			if corr > bestCorr {
				bestCorr = corr
				bestLag = lag
			}
		}

		if bestCorr > 0.3 {
			// Check for octave errors: prefer shorter period if it also has high correlation
			for lag := minPeriod; lag < bestLag; lag++ {
				var corr float64
				var energy float64
				for i := 0; i < maxPeriod && start+i < len(input) && start+i+lag < len(input); i++ {
					corr += input[start+i] * input[start+i+lag]
					energy += input[start+i] * input[start+i]
				}
				if energy > 1e-10 {
					corr /= energy
				}
				if corr > 0.7 && bestLag%lag == 0 {
					bestLag = lag
					break
				}
			}
			periods = append(periods, bestLag)
		} else {
			// Unvoiced: use a fallback period
			periods = append(periods, maxPeriod/2)
		}
	}

	if len(periods) == 0 {
		periods = append(periods, maxPeriod/2)
	}
	return periods
}

// simpleResampleShift is a fallback for unvoiced or short audio.
// Resamples by ratio using linear interpolation (pitch shift with duration change).
func simpleResampleShift(input []float64, ratio float64) []float64 {
	if math.Abs(ratio-1) < 0.001 {
		return input
	}
	n := len(input)
	if n < 2 {
		return input
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		srcPos := float64(i) / ratio
		srcIdx := int(srcPos)
		frac := srcPos - float64(srcIdx)
		if srcIdx >= n-1 {
			out[i] = input[n-1]
		} else if srcIdx < 0 {
			out[i] = input[0]
		} else {
			out[i] = input[srcIdx]*(1-frac) + input[srcIdx+1]*frac
		}
	}
	return out
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
func (p *Processor) SetPitchShift(semitones float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.PitchShift = semitones
}

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