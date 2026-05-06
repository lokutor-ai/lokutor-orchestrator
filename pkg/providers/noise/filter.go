package noise

import (
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// Filter is a real-time streaming noise suppressor.
type Filter struct {
	suppressor   *Suppressor
	inputBuffer  []float32
	olaBuffer    []float64
	fft          *fourier.FFT
	window       []float64
	prevBark     []float32
	dominantBand []int
	filterbank   [][]float32
}

// NewFilter creates a real-time noise filter from an ONNX model.
func NewFilter(modelPath string) (*Filter, error) {
	suppressor, err := NewSuppressor(modelPath)
	if err != nil {
		return nil, err
	}

	window := make([]float64, NFFT)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(NFFT))
	}

	filterbank := createBarkFilterbank(NFFT, SampleRate, NBands)

	dominantBand := make([]int, NFFT/2+1)
	for f := 0; f < NFFT/2+1; f++ {
		bestB, bestVal := 0, float32(0)
		for b := 0; b < NBands; b++ {
			if filterbank[b][f] > bestVal {
				bestVal = filterbank[b][f]
				bestB = b
			}
		}
		dominantBand[f] = bestB
	}

	return &Filter{
		suppressor:   suppressor,
		inputBuffer:  make([]float32, 0, NFFT*2),
		olaBuffer:    make([]float64, NFFT),
		fft:          fourier.NewFFT(NFFT),
		window:       window,
		prevBark:     make([]float32, NBands),
		dominantBand: dominantBand,
		filterbank:   filterbank,
	}, nil
}

// ProcessChunk processes a chunk of raw microphone audio and returns clean audio.
func (f *Filter) ProcessChunk(input []float32) []float32 {
	f.inputBuffer = append(f.inputBuffer, input...)
	output := make([]float32, 0, len(input))

	hiddenState := make([]float32, GRULayers*1*GRUUnits)

	for len(f.inputBuffer) >= NFFT {
		frame := f.inputBuffer[:NFFT]

		coeffs := f.frameSTFT(frame)
		features := f.extractFrameFeatures(coeffs, frame)

		gains, newHidden, err := f.suppressor.ProcessFrame(features, hiddenState)
		if err != nil {
			// On error, pass through
			output = append(output, frame[:HopLength]...)
			f.inputBuffer = f.inputBuffer[HopLength:]
			continue
		}
		hiddenState = newHidden

		enhanced := f.applyGains(coeffs, gains)
		f.addToOLA(enhanced)

		for i := 0; i < HopLength; i++ {
			denom := f.window[i] * f.window[i]
			if i+HopLength < NFFT {
				denom += f.window[i+HopLength] * f.window[i+HopLength]
			}
			if denom > 1e-10 {
				output = append(output, float32(f.olaBuffer[i]/denom))
			} else {
				output = append(output, float32(f.olaBuffer[i]))
			}
		}

		copy(f.olaBuffer, f.olaBuffer[HopLength:])
		for i := NFFT - HopLength; i < NFFT; i++ {
			f.olaBuffer[i] = 0
		}

		f.inputBuffer = f.inputBuffer[HopLength:]
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

func (f *Filter) extractFrameFeatures(coeffs []complex128, rawFrame []float32) []float32 {
	mag := make([]float32, NFFT/2+1)
	for i := range mag {
		mag[i] = float32(cmplx.Abs(coeffs[i]))
	}

	// Bark energies
	bark := make([]float32, NBands)
	for b := 0; b < NBands; b++ {
		var sum float32
		for freq := 0; freq < NFFT/2+1; freq++ {
			pow := mag[freq] * mag[freq]
			sum += pow * f.filterbank[b][freq]
		}
		bark[b] = float32(math.Log(float64(sum) + 1e-10))
	}

	// Pitch features
	pitch := f.pitchFeatures(rawFrame)

	// Deltas
	deltas := make([]float32, NBands)
	for b := 0; b < NBands; b++ {
		deltas[b] = bark[b] - f.prevBark[b]
	}
	copy(f.prevBark, bark)

	// Global features
	globals := f.globalFeatures(mag)

	feat := make([]float32, 0, NFeatures)
	feat = append(feat, bark...)
	feat = append(feat, pitch...)
	feat = append(feat, deltas...)
	feat = append(feat, globals...)
	return feat
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

	minLag := 40
	maxLag := 200
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
		func() float32 {
			if pitchStrength > 0.5 {
				return 1.0
			}
			return 0.0
		}(),
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

func (f *Filter) applyGains(coeffs []complex128, gains []float32) []complex128 {
	result := make([]complex128, len(coeffs))
	for i := range coeffs {
		g := gains[f.dominantBand[i]]
		if g < 0.01 {
			g = 0.01
		}
		if g > 1.0 {
			g = 1.0
		}
		result[i] = coeffs[i] * complex(float64(g), 0)
	}
	return result
}

func (f *Filter) addToOLA(coeffs []complex128) {
	frame := f.fft.Sequence(nil, coeffs)
	for i := 0; i < NFFT; i++ {
		f.olaBuffer[i] += frame[i] * f.window[i] / float64(NFFT)
	}
}

// Destroy cleans up resources.
func (f *Filter) Destroy() {
	if f.suppressor != nil {
		f.suppressor.Destroy()
	}
}

// Flush returns any remaining audio in the buffers.
func (f *Filter) Flush() []float32 {
	output := make([]float32, 0, len(f.inputBuffer)+NFFT)
	hiddenState := make([]float32, GRULayers*1*GRUUnits)

	if len(f.inputBuffer) > 0 {
		needed := NFFT - len(f.inputBuffer)
		if needed > 0 {
			f.inputBuffer = append(f.inputBuffer, make([]float32, needed)...)
		}

		frame := f.inputBuffer[:NFFT]
		coeffs := f.frameSTFT(frame)
		features := f.extractFrameFeatures(coeffs, frame)
		gains, newHidden, err := f.suppressor.ProcessFrame(features, hiddenState)
		if err == nil {
			hiddenState = newHidden
			enhanced := f.applyGains(coeffs, gains)
			f.addToOLA(enhanced)
		}

		for i := 0; i < HopLength; i++ {
			denom := f.window[i] * f.window[i]
			if i+HopLength < NFFT {
				denom += f.window[i+HopLength] * f.window[i+HopLength]
			}
			if denom > 1e-10 {
				output = append(output, float32(f.olaBuffer[i]/denom))
			} else {
				output = append(output, float32(f.olaBuffer[i]))
			}
		}

		copy(f.olaBuffer, f.olaBuffer[HopLength:])
		for i := NFFT - HopLength; i < NFFT; i++ {
			f.olaBuffer[i] = 0
		}
	}

	zeroFrame := make([]float32, NFFT)
	coeffs := f.frameSTFT(zeroFrame)
	features := f.extractFrameFeatures(coeffs, zeroFrame)
	gains, newHidden, err := f.suppressor.ProcessFrame(features, hiddenState)
	if err == nil {
		hiddenState = newHidden
		enhanced := f.applyGains(coeffs, gains)
		f.addToOLA(enhanced)
	}

	for i := 0; i < NFFT-HopLength; i++ {
		denom := f.window[i] * f.window[i]
		if i+HopLength < NFFT {
			denom += f.window[i+HopLength] * f.window[i+HopLength]
		}
		if denom > 1e-10 {
			output = append(output, float32(f.olaBuffer[i]/denom))
		} else {
			output = append(output, float32(f.olaBuffer[i]))
		}
	}

	return output
}
