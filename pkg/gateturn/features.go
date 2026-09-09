// Package gateturn wraps GateTurn, a ~17.5K-param dual-channel turn-taking
// model (github.com/danivarela/turn-taking), for production use in the
// orchestrator: a validated VAD replacement plus a duplex near/far barge-in
// classifier that can confirm or dismiss a tentative barge-in from raw audio
// alone, without waiting on STT.
//
// features.go is a straight, numerically-matched port of the Python
// reference's CausalFeatureExtractor (turn-taking/src/features.py) — same
// hop/window sizes, same mel filterbank construction, same autocorrelation
// pitch-confidence estimate, same stage-0 DSP skip. Verified against a
// golden fixture generated from the Python implementation
// (features_test.go).
package gateturn

import (
	"math"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	SampleRate = 16000
	Hop        = 320 // 20ms @ 16kHz
	Win        = 400 // 25ms @ 16kHz
	NMels      = 20
	NFFT       = 512
	// FeatureDim is one channel's feature width: N mel bins + energy_db/60 +
	// pitch confidence + spectral flux.
	FeatureDim = NMels + 3
	// EnergyIdx is the index of the energy_db/60 slot within one channel's
	// feature vector — must match model.py's ENERGY_IDX exactly, since the
	// duplex differential gate reads near/far energy straight out of the
	// feature window at this offset rather than from a separate input.
	EnergyIdx = FeatureDim - 3
)

// melFilterbank mirrors features.py's _mel_filterbank: NMels triangular
// filters over NFFT/2+1 power-spectrum bins, spaced on the mel scale between
// fmin and fmax.
func melFilterbank(fmin, fmax float64) [NMels][NFFT/2 + 1]float32 {
	hzToMel := func(f float64) float64 { return 2595.0 * math.Log10(1.0+f/700.0) }
	melToHz := func(m float64) float64 { return 700.0 * (math.Pow(10, m/2595.0) - 1.0) }

	melMin, melMax := hzToMel(fmin), hzToMel(fmax)
	nPts := NMels + 2
	melPts := make([]float64, nPts)
	for i := 0; i < nPts; i++ {
		melPts[i] = melMin + (melMax-melMin)*float64(i)/float64(nPts-1)
	}
	bins := make([]int, nPts)
	for i, m := range melPts {
		hz := melToHz(m)
		bins[i] = int(math.Floor(float64(NFFT+1) * hz / float64(SampleRate)))
	}

	var fb [NMels][NFFT/2 + 1]float32
	nBinsOut := NFFT/2 + 1
	for m := 1; m <= NMels; m++ {
		fLeft, fCenter, fRight := bins[m-1], bins[m], bins[m+1]
		for k := fLeft; k < fCenter; k++ {
			if k >= 0 && k < nBinsOut {
				denom := fCenter - fLeft
				if denom < 1 {
					denom = 1
				}
				fb[m-1][k] = float32(k-fLeft) / float32(denom)
			}
		}
		for k := fCenter; k < fRight; k++ {
			if k >= 0 && k < nBinsOut {
				denom := fRight - fCenter
				if denom < 1 {
					denom = 1
				}
				fb[m-1][k] = float32(fRight-k) / float32(denom)
			}
		}
	}
	return fb
}

// hannWindow mirrors np.hanning(win).
func hannWindow(n int) []float32 {
	w := make([]float32, n)
	if n == 1 {
		w[0] = 1
		return w
	}
	for i := 0; i < n; i++ {
		w[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}

// autocorrPitchConfidence mirrors features.py's _autocorr_pitch_confidence:
// a cheap voiced/pitch-confidence estimate via the normalized autocorrelation
// peak within the [fmin, fmax] lag range.
func autocorrPitchConfidence(frame []float32, fmin, fmax float64) float32 {
	n := len(frame)
	mean := float32(0)
	for _, s := range frame {
		mean += s
	}
	mean /= float32(n)

	centered := make([]float64, n)
	var energy float64
	for i, s := range frame {
		v := float64(s - mean)
		centered[i] = v
		energy += v * v
	}
	if energy < 1e-8 {
		return 0
	}

	lagMin := int(SampleRate / fmax)
	lagMax := int(SampleRate / fmin)
	if lagMax > n-1 {
		lagMax = n - 1
	}
	if lagMax <= lagMin {
		return 0
	}

	ac0 := energy // autocorrelation at lag 0 is just the energy
	peak := 0.0
	for lag := lagMin; lag < lagMax; lag++ {
		var sum float64
		for i := 0; i+lag < n; i++ {
			sum += centered[i] * centered[i+lag]
		}
		if sum > peak {
			peak = sum
		}
	}
	conf := peak / (ac0 + 1e-8)
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return float32(conf)
}

// CausalFeatureExtractor is a stateful, frame-by-frame feature extractor
// suitable for real-time streaming — a direct port of the Python class of
// the same purpose.
type CausalFeatureExtractor struct {
	fb     [NMels][NFFT/2 + 1]float32
	window []float32
	fft    *fourier.FFT

	buf         []float32 // ring buffer of the last Win samples
	prevMelLog  []float32
	havePrevMel bool

	prevFeat      []float32
	haveFeat      bool
	quietRun      int
	prevRawEnergy float32
	haveRawEnergy bool

	// scratch buffers reused across frames to avoid per-frame allocation on
	// the audio hot path.
	windowed []float64
	coeff    []complex128
	power    []float64
}

// NewCausalFeatureExtractor constructs an extractor with the same fmin/fmax
// as the Python default (50Hz-7600Hz mel range).
func NewCausalFeatureExtractor() *CausalFeatureExtractor {
	return &CausalFeatureExtractor{
		fb:       melFilterbank(50.0, 7600.0),
		window:   hannWindow(Win),
		fft:      fourier.NewFFT(NFFT),
		buf:      make([]float32, Win),
		windowed: make([]float64, NFFT),
		power:    make([]float64, NFFT/2+1),
	}
}

// Reset clears all state, matching Python's reset().
func (c *CausalFeatureExtractor) Reset() {
	for i := range c.buf {
		c.buf[i] = 0
	}
	c.havePrevMel = false
	c.haveFeat = false
	c.quietRun = 0
	c.haveRawEnergy = false
}

// RawEnergy is the stage-0 gate precursor: RMS energy straight off raw
// samples, no FFT. O(hop) adds.
func RawEnergy(samples []float32) float32 {
	var sumSq float64
	for _, s := range samples {
		sumSq += float64(s) * float64(s)
	}
	return float32(math.Sqrt(sumSq/float64(len(samples)) + 1e-9))
}

// PushFrameCascaded is the two-stage compute cascade's stage 0: compare raw
// energy to the previous frame's raw energy, and if the change is below
// threshold (and we haven't gone more than maxQuietRun frames without a
// refresh), skip the FFT/mel/pitch computation entirely and reuse the
// previous feature vector. Returns (feat, computed).
//
// samples must be exactly Hop (320) samples.
func (c *CausalFeatureExtractor) PushFrameCascaded(samples []float32, energyDeltaThresh float32, maxQuietRun int) ([]float32, bool) {
	e := RawEnergy(samples)
	havePrev := c.haveRawEnergy
	prevE := c.prevRawEnergy
	c.prevRawEnergy = e
	c.haveRawEnergy = true

	if c.haveFeat && havePrev {
		delta := e - prevE
		if delta < 0 {
			delta = -delta
		}
		if delta < energyDeltaThresh && c.quietRun < maxQuietRun {
			c.quietRun++
			// still must advance the ring buffer so state stays correct if
			// we resume real computation on a later frame.
			c.advanceBuf(samples)
			out := make([]float32, FeatureDim)
			copy(out, c.prevFeat)
			return out, false
		}
	}
	c.quietRun = 0
	feat := c.PushFrame(samples)
	if !c.haveFeat {
		c.prevFeat = make([]float32, FeatureDim)
		c.haveFeat = true
	}
	copy(c.prevFeat, feat)
	return feat, true
}

func (c *CausalFeatureExtractor) advanceBuf(samples []float32) {
	copy(c.buf, c.buf[Hop:])
	copy(c.buf[Win-Hop:], samples)
}

// PushFrame runs the full DSP frontend on exactly Hop new samples and
// returns the FeatureDim feature vector: NMels log-mel bins, energy_db/60,
// pitch confidence, spectral flux.
func (c *CausalFeatureExtractor) PushFrame(samples []float32) []float32 {
	c.advanceBuf(samples)

	for i, s := range c.buf {
		c.windowed[i] = float64(s * c.window[i])
	}
	for i := Win; i < NFFT; i++ {
		c.windowed[i] = 0
	}

	c.coeff = c.fft.Coefficients(c.coeff, c.windowed)
	for i, cv := range c.coeff {
		re, im := real(cv), imag(cv)
		c.power[i] = (re*re + im*im) / float64(NFFT)
	}

	feat := make([]float32, FeatureDim)
	for m := 0; m < NMels; m++ {
		var mel float64
		row := c.fb[m]
		for k, w := range row {
			if w != 0 {
				mel += float64(w) * c.power[k]
			}
		}
		feat[m] = float32(math.Log10(mel + 1e-6))
	}

	var meanSq float64
	for _, s := range c.buf {
		meanSq += float64(s) * float64(s)
	}
	meanSq /= float64(Win)
	energyDB := float32(10 * math.Log10(meanSq+1e-8))
	feat[NMels] = energyDB / 60.0

	pitchConf := autocorrPitchConfidence(c.buf, 70.0, 400.0)
	feat[NMels+1] = pitchConf

	var flux float32
	if c.havePrevMel {
		var sumSq float64
		for i := 0; i < NMels; i++ {
			d := feat[i] - c.prevMelLog[i]
			if d > 0 {
				sumSq += float64(d) * float64(d)
			}
		}
		flux = float32(math.Sqrt(sumSq / float64(NMels)))
	}
	feat[NMels+2] = flux

	if c.prevMelLog == nil {
		c.prevMelLog = make([]float32, NMels)
	}
	copy(c.prevMelLog, feat[:NMels])
	c.havePrevMel = true

	return feat
}
