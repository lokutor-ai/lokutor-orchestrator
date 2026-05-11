package audio

import "math"

type BackchannelType int

const (
	BackchannelMhm   BackchannelType = iota
	BackchannelUhHuh
)

func GenerateBackchannel(bt BackchannelType, sampleRate int) []int16 {
	switch bt {
	case BackchannelMhm:
		return generateMhm(sampleRate)
	case BackchannelUhHuh:
		return generateUhHuh(sampleRate)
	default:
		return generateMhm(sampleRate)
	}
}

func generateMhm(sampleRate int) []int16 {
	duration := 0.18
	n := int(float64(sampleRate) * duration)
	samples := make([]int16, n)
	baseFreq := 175.0

	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		modFreq := baseFreq * (1.0 - 0.12*math.Sin(math.Pi*t/duration))

		env := 1.0
		attack := 0.04
		release := 0.04
		if t < attack {
			env = t / attack
		} else if t > duration-release {
			env = (duration - t) / release
		}

		val := math.Sin(2*math.Pi*modFreq*t) +
			0.25*math.Sin(2*math.Pi*modFreq*2*t) +
			0.08*math.Sin(2*math.Pi*modFreq*3*t)
		val /= 1.33
		samples[i] = int16(val * env * 3500)
	}
	return samples
}

func generateUhHuh(sampleRate int) []int16 {
	duration := 0.28
	n := int(float64(sampleRate) * duration)
	samples := make([]int16, n)
	split := n / 2

	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		env := 1.0
		var val float64
		freq := 200.0

		if i < split {
			freq = 200.0
			attack := 0.025
			if t < attack {
				env = t / attack
			}
		} else {
			freq = 270.0
			relT := t - float64(split)/float64(sampleRate)
			maxT := duration - float64(split)/float64(sampleRate)
			release := 0.04
			if relT > maxT-release {
				env = (maxT - relT) / release
			}
			env *= 0.85
		}

		val = math.Sin(2*math.Pi*freq*t) + 0.15*math.Sin(2*math.Pi*freq*2*t)
		val /= 1.15
		samples[i] = int16(val * env * 3000)
	}
	return samples
}
