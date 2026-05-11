package noise

import (
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// Filter is a real-time streaming noise suppressor with center=True semantics.
type Filter struct {
	suppressor    *Suppressor
	inputBuffer   []float32
	olaBuffer     []float64
	windowSum     []float64
	fft           *fourier.FFT
	window        []float64
	prevBark      []float32
	hiddenState   []float32
	rawFilterbank [][]float32
	normFilterbank [][]float32
	dfBinIndices  []int
	padSize       int
	padded        bool
	padAccum      []float32
	droppedPad    bool

	VADThreshold  float32
	VADHoldFrames int
	_vadBelow     int
}

// NewFilter creates a real-time noise filter with center=True.
func NewFilter(modelPath string) (*Filter, error) {
	suppressor, err := NewSuppressor(modelPath)
	if err != nil {
		return nil, err
	}

	window := make([]float64, NFFT)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(NFFT))
	}

	rawFB := createBarkFilterbank(NFFT, SampleRate, NBands)
	normFB := normalizeFilterbank(rawFB)
	padSize := NFFT / 2

	return &Filter{
		suppressor:    suppressor,
		inputBuffer:   make([]float32, padSize, NFFT*3),
		olaBuffer:     make([]float64, NFFT),
		windowSum:     make([]float64, NFFT),
		fft:           fourier.NewFFT(NFFT),
		window:        window,
		prevBark:      make([]float32, NBands),
		hiddenState:   make([]float32, GRULayers*1*GRUUnits),
		rawFilterbank: rawFB,
		normFilterbank: normFB,
		dfBinIndices:  getDFBinIndices(),
		padSize:       padSize,
		padded:        false,
		padAccum:      nil,
		droppedPad:    false,
		VADThreshold:  0.3,
		VADHoldFrames: 3,
	}, nil
}

// ProcessChunk processes audio and returns denoised output.
func (f *Filter) ProcessChunk(input []float32) []float32 {
	// On first call, fill padding buffer with reflection of input start
	if !f.padded && len(input) >= f.padSize {
		for i := 0; i < f.padSize; i++ {
			refIdx := i + 1
			if refIdx < len(input) {
				f.inputBuffer[f.padSize-1-i] = input[refIdx]
			} else {
				f.inputBuffer[f.padSize-1-i] = input[len(input)-1-i]
			}
		}
		f.padded = true
	}

	f.inputBuffer = append(f.inputBuffer, input...)
	out := f.processFrames()

	// Accumulate and drop padding samples
	f.padAccum = append(f.padAccum, out...)
	if !f.droppedPad && len(f.padAccum) >= f.padSize {
		f.padAccum = f.padAccum[f.padSize:]
		f.droppedPad = true
	}
	if !f.droppedPad {
		return nil
	}

	result := f.padAccum
	f.padAccum = nil
	return result
}

// Flush processes remaining audio and flushes OLA.
func (f *Filter) Flush() []float32 {
	out := f.processZeroFrames(NFFT / HopLength)
	if cap(f.inputBuffer) >= f.padSize {
		f.inputBuffer = f.inputBuffer[:f.padSize]
	} else {
		f.inputBuffer = make([]float32, f.padSize, NFFT*3)
	}
	f.padded = false
	f.hiddenState = make([]float32, GRULayers*1*GRUUnits)
	f.droppedPad = false
	f.padAccum = nil
	f.prevBark = make([]float32, NBands)
	f.olaBuffer = make([]float64, NFFT)
	f.windowSum = make([]float64, NFFT)
	f._vadBelow = 0
	return out
}

// Destroy cleans up resources.
func (f *Filter) Destroy() {
	if f.suppressor != nil {
		f.suppressor.Destroy()
	}
}

func (f *Filter) processFrames() []float32 {
	output := make([]float32, 0, HopLength)

	for len(f.inputBuffer) >= NFFT+f.padSize {
		frame := f.inputBuffer[:NFFT]
		coeffs := f.frameSTFT(frame)
		bark, pitch, globals := f.extractFrameFeatures(coeffs, frame)

		deltas := make([]float32, NBands)
		for b := 0; b < NBands; b++ {
			deltas[b] = bark[b] - f.prevBark[b]
		}
		copy(f.prevBark, bark)

		features := make([]float32, 0, NFeatures)
		features = append(features, bark...)
		features = append(features, pitch...)
		features = append(features, deltas...)
		features = append(features, globals...)

		gains, dfCoefs, vad, newHidden, err := f.suppressor.ProcessFrame(features, f.hiddenState)
		if err != nil {
			f.inputBuffer = f.inputBuffer[HopLength:]
			continue
		}
		f.hiddenState = newHidden

		if vad < f.VADThreshold {
			f._vadBelow++
		} else {
			f._vadBelow = 0
		}
		silenceFrame := f._vadBelow >= f.VADHoldFrames

		if !silenceFrame {
			enhanced := f.applyEnhancement(coeffs, gains, dfCoefs)
			f.addToOLA(enhanced)
		} else {
			f.addToOLA(coeffs)
		}

		for i := 0; i < HopLength; i++ {
			if f.windowSum[i] > 1e-6 {
				output = append(output, float32(f.olaBuffer[i]/f.windowSum[i]))
			} else if f.windowSum[i] > 1e-10 {
				output = append(output, float32(f.olaBuffer[i]))
			} else {
				output = append(output, 0)
			}
		}

		if silenceFrame {
			for i := len(output) - HopLength; i < len(output); i++ {
				output[i] = 0
			}
		}

		copy(f.olaBuffer, f.olaBuffer[HopLength:])
		copy(f.windowSum, f.windowSum[HopLength:])
		for i := NFFT - HopLength; i < NFFT; i++ {
			f.olaBuffer[i] = 0
			f.windowSum[i] = 0
		}
		f.inputBuffer = f.inputBuffer[HopLength:]
	}

	return output
}

func (f *Filter) processZeroFrames(count int) []float32 {
	output := make([]float32, 0, count*HopLength)

	for i := 0; i < count; i++ {
		zeroFrame := make([]float32, NFFT)
		if len(f.inputBuffer) >= NFFT {
			copy(zeroFrame, f.inputBuffer[:NFFT])
		} else if len(f.inputBuffer) > 0 {
			copy(zeroFrame, f.inputBuffer)
		}

		coeffs := f.frameSTFT(zeroFrame)
		bark, pitch, globals := f.extractFrameFeatures(coeffs, zeroFrame)

		deltas := make([]float32, NBands)
		for b := 0; b < NBands; b++ {
			deltas[b] = bark[b] - f.prevBark[b]
		}
		copy(f.prevBark, bark)

		features := make([]float32, 0, NFeatures)
		features = append(features, bark...)
		features = append(features, pitch...)
		features = append(features, deltas...)
		features = append(features, globals...)

		gains, dfCoefs, _, newHidden, err := f.suppressor.ProcessFrame(features, f.hiddenState)
		if err == nil {
			f.hiddenState = newHidden
			enhanced := f.applyEnhancement(coeffs, gains, dfCoefs)
			f.addToOLA(enhanced)
		}

		for j := 0; j < HopLength; j++ {
			if f.windowSum[j] > 1e-6 {
				output = append(output, float32(f.olaBuffer[j]/f.windowSum[j]))
			} else if f.windowSum[j] > 1e-10 {
				output = append(output, float32(f.olaBuffer[j]))
			} else {
				output = append(output, 0)
			}
		}

		copy(f.olaBuffer, f.olaBuffer[HopLength:])
		copy(f.windowSum, f.windowSum[HopLength:])
		for j := NFFT - HopLength; j < NFFT; j++ {
			f.olaBuffer[j] = 0
			f.windowSum[j] = 0
		}

		if len(f.inputBuffer) > 0 {
			if len(f.inputBuffer) >= HopLength {
				f.inputBuffer = f.inputBuffer[HopLength:]
			} else {
				f.inputBuffer = f.inputBuffer[:0]
			}
		}
	}

	return output
}

func (f *Filter) frameSTFT(frame []float32) []complex128 {
	dFrame := make([]float64, NFFT)
	for i := range frame {
		dFrame[i] = float64(frame[i]) * f.window[i]
	}
	return f.fft.Coefficients(nil, dFrame)
}

func (f *Filter) extractFrameFeatures(coeffs []complex128, rawFrame []float32) (bark, pitch, globals []float32) {
	mag := make([]float32, NFFT/2+1)
	for i := range mag {
		mag[i] = float32(cmplx.Abs(coeffs[i]))
	}

	bark = make([]float32, NBands)
	for b := 0; b < NBands; b++ {
		var sum float32
		for freq := 0; freq < NFFT/2+1; freq++ {
			pow := mag[freq] * mag[freq]
			sum += pow * f.rawFilterbank[b][freq]
		}
		bark[b] = float32(math.Log(float64(sum) + 1e-10))
	}

	pitch = f.pitchFeatures(rawFrame)
	globals = f.globalFeatures(mag)
	return bark, pitch, globals
}

func (f *Filter) pitchFeatures(frame []float32) []float32 {
	n := len(frame)
	acf := make([]float32, n)
	for lag := 0; lag < n; lag++ {
		var sum float32
		for i := 0; i < n-lag; i++ {
			sum += frame[i] * frame[i+lag]
		}
		acf[lag] = sum
	}
	zeroLag := acf[0]
	if zeroLag < 1e-10 {
		zeroLag = 1e-10
	}
	minLag, maxLag := 40, 200
	if maxLag > n {
		maxLag = n
	}
	bestLag := minLag
	bestCorr := float32(0)
	for lag := minLag; lag < maxLag; lag++ {
		if acf[lag] > bestCorr {
			bestCorr = acf[lag]
			bestLag = lag
		}
	}
	pitchStrength := bestCorr / zeroLag
	pitchPeriod := float32(bestLag)
	pitchFreq := float32(SampleRate) / pitchPeriod
	return []float32{
		pitchPeriod / 200.0,
		pitchStrength,
		pitchFreq / 500.0,
		float32(math.Log1p(float64(pitchPeriod))) / 5.0,
		func() float32 { if pitchStrength > 0.5 { return 1.0 }; return 0.0 }(),
		float32(math.Log1p(float64(zeroLag))) / 10.0,
	}
}

func (f *Filter) globalFeatures(mag []float32) []float32 {
	nFreq := len(mag)
	var energy, weightedSum, sum, logSum float32
	for freq := 0; freq < nFreq; freq++ {
		energy += mag[freq] * mag[freq]
		fVal := float32(freq) * float32(SampleRate/2) / float32(nFreq-1)
		weightedSum += fVal * mag[freq]
		sum += mag[freq]
		if mag[freq] > 0 {
			logSum += float32(math.Log(float64(mag[freq])))
		}
	}
	centroid := float32(0)
	if sum > 1e-10 {
		centroid = weightedSum / sum
	}
	gm := float32(math.Exp(float64(logSum) / float64(nFreq)))
	am := sum / float32(nFreq)
	flatness := float32(0)
	if am > 1e-10 {
		flatness = gm / am
	}
	lowRatio := float32(0)
	if nFreq > 1 && mag[0]+mag[1] > 1e-10 {
		lowRatio = mag[1] / (mag[0] + mag[1])
	}
	return []float32{
		float32(math.Log1p(float64(energy))) / 10.0,
		centroid / float32(SampleRate/2),
		flatness,
		lowRatio,
	}
}

func (f *Filter) applyEnhancement(coeffs []complex128, gains, dfCoefs []float32) []complex128 {
	result := make([]complex128, len(coeffs))

	gainPerFreq := make([]float32, NFFT/2+1)
	for freq := 0; freq < NFFT/2+1; freq++ {
		var g float32
		for b := 0; b < NBands; b++ {
			g += gains[b] * f.normFilterbank[b][freq]
		}
		if g < 0 {
			g = 0
		}
		if g > 1.0 {
			g = 1.0
		}
		gainPerFreq[freq] = g
	}

	multReal := make([]float32, NFFT/2+1)
	copy(multReal, gainPerFreq)
	multImag := make([]float32, NFFT/2+1)

	for i, binIdx := range f.dfBinIndices {
		if binIdx >= NFFT/2+1 {
			continue
		}
		g := gainPerFreq[binIdx]
		multReal[binIdx] = g * dfCoefs[i*2]
		multImag[binIdx] = g * dfCoefs[i*2+1]
	}

	for freq := 0; freq < NFFT/2+1; freq++ {
		result[freq] = coeffs[freq] * complex(float64(multReal[freq]), float64(multImag[freq]))
	}

	return result
}

func (f *Filter) addToOLA(coeffs []complex128) {
	frame := f.fft.Sequence(nil, coeffs)
	for i := 0; i < NFFT; i++ {
		f.olaBuffer[i] += frame[i] * f.window[i] / float64(NFFT)
		f.windowSum[i] += f.window[i] * f.window[i]
	}
}


