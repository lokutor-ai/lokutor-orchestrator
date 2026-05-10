package audio

import (
	"math"
)

const (
	minPitchHz      = 50.0
	maxPitchHz      = 500.0
	voicedThreshold = 0.30
	silenceRMS      = 500.0
)

func DetectPitch(samples []int16, sampleRate int) float64 {
	if len(samples) < sampleRate/minPitchHz/2 {
		return 0
	}

	n := len(samples)
	floats := make([]float64, n)
	var sum float64
	for i, s := range samples {
		floats[i] = float64(s)
		sum += floats[i]
	}
	mean := sum / float64(n)
	for i := range floats {
		floats[i] -= mean
	}

	var energy float64
	for _, f := range floats {
		energy += f * f
	}
	rms := math.Sqrt(energy / float64(n))
	if rms < silenceRMS {
		return 0
	}

	minLag := sampleRate / int(maxPitchHz)
	if minLag < 1 {
		minLag = 1
	}
	maxLag := sampleRate / int(minPitchHz)
	if maxLag > n/2 {
		maxLag = n / 2
	}
	if maxLag <= minLag {
		return 0
	}

	bestLag := 0
	bestCorr := 0.0
	for lag := minLag; lag <= maxLag; lag++ {
		var corr, e1, e2 float64
		for i := 0; i < n-lag; i++ {
			corr += floats[i] * floats[i+lag]
			e1 += floats[i] * floats[i]
			e2 += floats[i+lag] * floats[i+lag]
		}
		denom := math.Sqrt(e1 * e2)
		if denom > 0 {
			corr /= denom
		}
		if corr > bestCorr {
			bestCorr = corr
			bestLag = lag
		}
	}

	if bestCorr < voicedThreshold || bestLag <= 0 {
		return 0
	}

	return float64(sampleRate) / float64(bestLag)
}
