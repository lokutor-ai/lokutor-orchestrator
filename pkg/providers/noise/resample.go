package noise

import (
	"math"
)

// ResampleLinear performs simple linear interpolation resampling.
func ResampleLinear(input []float32, inputRate, outputRate int) []float32 {
	if inputRate == outputRate {
		out := make([]float32, len(input))
		copy(out, input)
		return out
	}
	
	ratio := float64(outputRate) / float64(inputRate)
	outputLen := int(float64(len(input)) * ratio)
	output := make([]float32, outputLen)
	
	for i := range output {
		srcIdx := float64(i) / ratio
		idx0 := int(math.Floor(srcIdx))
		idx1 := idx0 + 1
		frac := float32(srcIdx - float64(idx0))
		
		if idx0 >= len(input) {
			output[i] = input[len(input)-1]
		} else if idx1 >= len(input) {
			output[i] = input[idx0]
		} else {
			output[i] = input[idx0]*(1-frac) + input[idx1]*frac
		}
	}
	
	return output
}
