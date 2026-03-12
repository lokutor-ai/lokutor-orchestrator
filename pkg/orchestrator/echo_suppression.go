package orchestrator

import (
	"math"
	"sync"
	"time"
)

type EchoSuppressor struct {
	mu                     sync.Mutex
	playedSamples          []float64
	writeIdx               int
	count                  int
	maxSamples             int
	echoThreshold          float64
	echoSilenceMS          int
	lastTTSTime            time.Time
	enabled                bool
	recentPlaybackWindowMS int

	playbackSampleRate int
	inputSampleRate    int
}

func (es *EchoSuppressor) getRecentSamplesInternal(limit int) []float64 {
	if es.count == 0 {
		return nil
	}
	n := es.count
	if limit > 0 && limit < n {
		n = limit
	}

	out := make([]float64, n)
	for i := 0; i < n; i++ {
		idx := (es.writeIdx - n + i + es.maxSamples) % es.maxSamples
		out[i] = es.playedSamples[idx]
	}
	return out
}

func (es *EchoSuppressor) getRecentSamples(limit int) []float64 {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.getRecentSamplesInternal(limit)
}

func (es *EchoSuppressor) maxCorrelationSamples(inputSamples, refSamples []float64) float64 {
	if len(inputSamples) == 0 || len(refSamples) == 0 {
		return 0
	}

	compareLen := len(inputSamples)
	if compareLen > len(refSamples) {
		compareLen = len(refSamples)
	}

	inputEnergy := calculateEnergy(inputSamples[:compareLen])
	if inputEnergy < 1e-12 {
		return 0
	}

	maxCorr := 0.0
	stride := compareLen / 4
	if stride < 16 {
		stride = 16
	}

	searchRange := len(refSamples) - compareLen + 1
	for pos := 0; pos < searchRange; pos += stride {
		segEnergy := 0.0
		dot := 0.0

		seg := refSamples[pos : pos+compareLen]
		for i := 0; i < compareLen; i++ {
			segEnergy += seg[i] * seg[i]
			dot += inputSamples[i] * seg[i]
		}

		if segEnergy > 1e-12 {
			corr := dot / math.Sqrt(inputEnergy*segEnergy)
			if corr > maxCorr {
				maxCorr = corr
			}
		}
	}

	if maxCorr < 0 {
		maxCorr = 0
	} else if maxCorr > 1 {
		maxCorr = 1
	}
	return maxCorr
}

func NewEchoSuppressor() *EchoSuppressor {
	return NewEchoSuppressorWithRates(44100, 44100)
}

func NewEchoSuppressorWithConfig(config Config) *EchoSuppressor {
	es := NewEchoSuppressorWithRates(config.SampleRate, config.SampleRate)
	if config.EchoSuppressionThreshold > 0 {
		es.echoThreshold = config.EchoSuppressionThreshold
	}
	return es
}

func NewEchoSuppressorWithRates(playbackRate, inputRate int) *EchoSuppressor {
	if playbackRate <= 0 {
		playbackRate = 44100
	}
	if inputRate <= 0 {
		inputRate = playbackRate
	}
	maxSamples := playbackRate * 2
	return &EchoSuppressor{
		playedSamples:          make([]float64, maxSamples),
		maxSamples:             maxSamples,
		echoThreshold:          0.80,
		echoSilenceMS:          2000,
		recentPlaybackWindowMS: 2000,
		enabled:                true,
		playbackSampleRate:     playbackRate,
		inputSampleRate:        inputRate,
	}
}

func (es *EchoSuppressor) RecordPlayedAudio(chunk []byte) {
	if !es.enabled || len(chunk) == 0 {
		return
	}

	samples := bytesToSamples(chunk)
	if len(samples) == 0 {
		return
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	for _, s := range samples {
		es.playedSamples[es.writeIdx] = s
		es.writeIdx = (es.writeIdx + 1) % es.maxSamples
		if es.count < es.maxSamples {
			es.count++
		}
	}
	es.lastTTSTime = time.Now()
}

func (es *EchoSuppressor) getSampleAt(i int) float64 {
	idx := (es.writeIdx - es.count + i + es.maxSamples) % es.maxSamples
	return es.playedSamples[idx]
}

func (es *EchoSuppressor) IsEcho(inputChunk []byte) bool {
	return es.isEchoImpl(inputChunk, false)
}

func (es *EchoSuppressor) IsEchoFast(inputChunk []byte) bool {
	return es.isEchoImpl(inputChunk, true)
}

func (es *EchoSuppressor) isEchoImpl(inputChunk []byte, fast bool) bool {
	if !es.enabled || len(inputChunk) == 0 {
		return false
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	if time.Since(es.lastTTSTime) > time.Duration(es.echoSilenceMS)*time.Millisecond {
		return false
	}

	searchSize := es.count
	if fast {
		// macOS CoreAudio and Bluetooth devices can have up to 800-1200ms of playback queue delay
		// so we need at least a 1.5s search window to catch the echo.
		maxWindow := int(float64(es.playbackSampleRate) * 1.5)
		if searchSize > maxWindow {
			searchSize = maxWindow
		}
	}

	threshold := es.echoThreshold
	inputSamples := bytesToSamples(inputChunk)
	if es.inputSampleRate != es.playbackSampleRate {
		inputSamples = resample(inputSamples, es.inputSampleRate, es.playbackSampleRate)
	}
	correlation := es.maxCorrelationRing(inputSamples, searchSize, fast)

	if correlation > threshold {
		return true
	}

	envCorr := es.maxEnvelopeCorrelationRing(inputSamples, searchSize, 8)
	return envCorr > threshold+0.05
}

func (es *EchoSuppressor) maxCorrelationRing(inputSamples []float64, searchSize int, fast bool) float64 {
	if len(inputSamples) == 0 || searchSize == 0 {
		return 0
	}

	compareLen := len(inputSamples)
	if compareLen > searchSize {
		compareLen = searchSize
	}

	inputEnergy := calculateEnergy(inputSamples[:compareLen])
	if inputEnergy < 1e-12 {
		return 0
	}

	maxCorr := 0.0
	stride := 64 // Use fixed resolution for better reliability
	if fast {
		stride = 128
	}

	searchRange := searchSize - compareLen + 1
	for pos := 0; pos < searchRange; pos += stride {
		dot := 0.0
		segEnergy := 0.0

		for i := 0; i < compareLen; i++ {
			s := es.getSampleAt(pos + i)
			dot += inputSamples[i] * s
			segEnergy += s * s
		}

		if segEnergy > 1e-12 {
			corr := dot / math.Sqrt(inputEnergy*segEnergy)
			if corr > maxCorr {
				maxCorr = corr
				if maxCorr >= 0.999 {
					return maxCorr
				}
			}
		}
	}

	if maxCorr < 0 {
		maxCorr = 0
	} else if maxCorr > 1 {
		maxCorr = 1
	}
	return maxCorr
}

func (es *EchoSuppressor) maxEnvelopeCorrelationRing(inSamples []float64, searchSize int, decimation int) float64 {
	if len(inSamples) == 0 || searchSize == 0 {
		return 0
	}

	inEnvLen := len(inSamples) / decimation
	if inEnvLen == 0 {
		return 0
	}

	inEnv := make([]float64, inEnvLen)
	for i := 0; i < inEnvLen; i++ {
		sum := 0.0
		for j := 0; j < decimation; j++ {
			sum += math.Abs(inSamples[i*decimation+j])
		}
		inEnv[i] = sum
	}

	refEnvLen := searchSize / decimation
	if refEnvLen == 0 {
		return 0
	}

	refEnv := make([]float64, refEnvLen)
	for i := 0; i < refEnvLen; i++ {
		sum := 0.0
		for j := 0; j < decimation; j++ {
			sum += math.Abs(es.getSampleAt(i*decimation + j))
		}
		refEnv[i] = sum
	}

	compareLen := inEnvLen
	if compareLen > refEnvLen {
		compareLen = refEnvLen
	}

	inMean := 0.0
	for i := 0; i < compareLen; i++ {
		inMean += inEnv[i]
	}
	inMean /= float64(compareLen)

	inVar := 0.0
	for i := 0; i < compareLen; i++ {
		inEnv[i] -= inMean
		inVar += inEnv[i] * inEnv[i]
	}

	if inVar <= 1e-12 {
		return 0
	}

	maxCorr := 0.0
	stride := 128
	if stride < 4 {
		stride = 4
	}

	searchRange := refEnvLen - compareLen + 1

	for pos := 0; pos < searchRange; pos += stride {
		refMean := 0.0
		for i := 0; i < compareLen; i++ {
			refMean += refEnv[pos+i]
		}
		refMean /= float64(compareLen)

		dot := 0.0
		refVar := 0.0
		for i := 0; i < compareLen; i++ {
			r := refEnv[pos+i] - refMean
			dot += inEnv[i] * r
			refVar += r * r
		}

		if refVar > 1e-12 {
			corr := dot / math.Sqrt(inVar*refVar)
			if corr > maxCorr {
				maxCorr = corr
			}
		}
	}

	return maxCorr
}

func bytesToSamples(data []byte) []float64 {
	samples := make([]float64, 0, len(data)/2)
	for i := 0; i < len(data)-1; i += 2 {
		sample := int16(data[i]) | (int16(data[i+1]) << 8)
		normalized := float64(sample) / 32768.0
		samples = append(samples, normalized)
	}
	return samples
}

func resample(samples []float64, inRate, outRate int) []float64 {
	if len(samples) == 0 || inRate == outRate || inRate <= 0 || outRate <= 0 {
		return samples
	}
	ratio := float64(outRate) / float64(inRate)
	newLen := int(float64(len(samples))*ratio + 0.5)
	if newLen <= 0 {
		return nil
	}
	out := make([]float64, newLen)
	for i := 0; i < newLen; i++ {
		pos := float64(i) / ratio
		idx := int(pos)
		frac := pos - float64(idx)
		if idx+1 < len(samples) {
			out[i] = samples[idx]*(1-frac) + samples[idx+1]*frac
		} else {
			out[i] = samples[idx]
		}
	}
	return out
}

func calculateEnergy(samples []float64) float64 {
	energy := 0.0
	for _, s := range samples {
		energy += s * s
	}
	return energy
}

func (es *EchoSuppressor) ClearEchoBuffer() {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.writeIdx = 0
	es.count = 0
}

func (es *EchoSuppressor) PostProcess(input []byte) []byte {
	if !es.enabled || len(input) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	const frameMs = 20

	es.mu.Lock()
	searchSize := es.count
	threshold := es.echoThreshold
	inputSamples := bytesToSamples(input)
	out := make([]byte, len(input))
	copy(out, input)

	inputRate := es.inputSampleRate
	if inputRate <= 0 {
		inputRate = 44100
	}
	frameSamples := (inputRate * frameMs) / 1000
	for i := 0; i < len(inputSamples); i += frameSamples {
		end := i + frameSamples
		if end > len(inputSamples) {
			end = len(inputSamples)
		}
		frame := inputSamples[i:end]

		if es.inputSampleRate != es.playbackSampleRate {
			frame = resample(frame, es.inputSampleRate, es.playbackSampleRate)
		}

		corr := es.maxCorrelationRing(frame, searchSize, true)
		if corr > threshold {
			for j := i * 2; j < end*2 && j < len(out); j++ {
				out[j] = 0
			}
		}
	}
	es.mu.Unlock()

	return out
}

func (es *EchoSuppressor) RemoveEchoRealtime(input []byte) []byte {
	if !es.enabled || len(input) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	if time.Since(es.lastTTSTime) > time.Duration(es.echoSilenceMS)*time.Millisecond {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	searchSize := es.count

	if searchSize == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	inSamples := bytesToSamples(input)
	if es.inputSampleRate != es.playbackSampleRate {
		inSamples = resample(inSamples, es.inputSampleRate, es.playbackSampleRate)
	}
	maxCorr := es.maxCorrelationRing(inSamples, searchSize, true)

	// Only hard-mute if correlation is EXTREMELY high (0.95+). 
	// This prevents zeroing out a user speaking over the bot.
	if maxCorr < 0.95 {
		envCorr := es.maxEnvelopeCorrelationRing(inSamples, searchSize, 8)
		if envCorr < 0.98 {
			out := make([]byte, len(input))
			copy(out, input)
			return out
		}
	}

	return make([]byte, len(input))
}

func (es *EchoSuppressor) SetThreshold(threshold float64) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if threshold >= 0 && threshold <= 1 {
		es.echoThreshold = threshold
	}
}

func (es *EchoSuppressor) SetPlaybackSampleRate(rate int) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if rate <= 0 {
		return
	}
	if rate == es.playbackSampleRate {
		return
	}
	es.playbackSampleRate = rate
	newMax := rate * 2
	if newMax != es.maxSamples {
		es.playedSamples = make([]float64, newMax)
		es.maxSamples = newMax
		es.writeIdx = 0
		es.count = 0
	}
}

func (es *EchoSuppressor) SetInputSampleRate(rate int) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if rate <= 0 {
		return
	}
	es.inputSampleRate = rate
}

func (es *EchoSuppressor) SetSampleRates(playbackRate, inputRate int) {
	es.SetPlaybackSampleRate(playbackRate)
	es.SetInputSampleRate(inputRate)
}

func (es *EchoSuppressor) SetEnabled(enabled bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.enabled = enabled
}
