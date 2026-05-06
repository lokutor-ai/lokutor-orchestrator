package noise

import (
	"math"
)

// createBarkFilterbank creates a triangular Bark-scale filterbank.
func createBarkFilterbank(nFFT, sr, nBands int) [][]float32 {
	nFreq := nFFT/2 + 1
	freqs := make([]float64, nFreq)
	for i := range freqs {
		freqs[i] = float64(i) * float64(sr/2) / float64(nFreq-1)
	}

	bark := make([]float64, nFreq)
	for i := range bark {
		f := freqs[i]
		bark[i] = 13.0*math.Atan(0.00076*f) + 3.5*math.Atan((f/7500.0)*(f/7500.0))
	}

	barkEdges := make([]float64, nBands+2)
	for i := range barkEdges {
		barkEdges[i] = bark[0] + float64(i)*(bark[nFreq-1]-bark[0])/float64(nBands+1)
	}

	filterbank := make([][]float32, nBands)
	for i := range filterbank {
		filterbank[i] = make([]float32, nFreq)
		left := barkEdges[i]
		center := barkEdges[i+1]
		right := barkEdges[i+2]
		for j, b := range bark {
			if left <= b && b <= center {
				filterbank[i][j] = float32((b - left) / (center - left + 1e-10))
			} else if center < b && b <= right {
				filterbank[i][j] = float32((right - b) / (right - center + 1e-10))
			}
		}
	}
	return filterbank
}

// hzToBark converts Hz to Bark scale.
func hzToBark(f float64) float64 {
	return 13.0*math.Atan(0.00076*f) + 3.5*math.Atan((f/7500.0)*(f/7500.0))
}
