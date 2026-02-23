package orchestrator

import (
	"math"
	"sync"
	"time"
)

// EchoSuppressor detects and filters out speaker echo from microphone input.
// It uses correlation-based analysis to detect when input audio matches recently
// played audio.  Both the playback sample rate (audio sent to speakers) and the
// input sample rate (microphone stream) can be configured; the echo suppressor
// will internally resample the smaller-rate stream to match the larger when
// performing correlation checks.  This makes it possible for example to record
// the output at 44 kHz while the mic runs at 16 kHz.
type EchoSuppressor struct {
	mu            sync.Mutex
	playedSamples []float64 // Ring buffer of played samples
	writeIdx      int       // Current write position in ring buffer
	count         int       // Number of samples currently in buffer
	maxSamples    int       // Max number of samples to store
	echoThreshold float64   // Correlation threshold above which we consider audio to be echo
	echoSilenceMS int       // How long to suppress echoes after TTS stops (ms)
	lastTTSTime   time.Time // When we last played audio
	enabled       bool
	// For real-time detection we also keep a short recent-playback duration to
	// tolerate playback-to-mic latency (ms).
	recentPlaybackWindowMS int

	// sample rate configuration (in Hz). playbackSampleRate is the rate
	// of the audio sent to the speakers; inputSampleRate is the rate of the
	// microphone stream. They may differ, in which case the input will be
	// resampled internally before correlation checks.
	playbackSampleRate int
	inputSampleRate    int
}

// getRecentSamplesInternal returns a linear slice of the most recent samples.
// caller MUST hold es.mu
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

// getRecentSamples returns a linear slice of the most recent samples.
func (es *EchoSuppressor) getRecentSamples(limit int) []float64 {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.getRecentSamplesInternal(limit)
}

// maxCorrelationSamples performs a sliding-window search.
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

// NewEchoSuppressor creates a new echo suppressor at the common 44.1kHz rate.
// It is retained for backwards compatibility. For custom rates use
// NewEchoSuppressorWithRates.
func NewEchoSuppressor() *EchoSuppressor {
	return NewEchoSuppressorWithRates(44100, 44100)
}

// NewEchoSuppressorWithRates creates an echo suppressor configured for the
// provided playback and input sample rates. The internal reference buffer is
// sized for roughly two seconds of playback audio.
func NewEchoSuppressorWithRates(playbackRate, inputRate int) *EchoSuppressor {
	if playbackRate <= 0 {
		playbackRate = 44100
	}
	if inputRate <= 0 {
		inputRate = playbackRate
	}
	// ~2 seconds worth of samples at playback rate
	maxSamples := playbackRate * 2
	return &EchoSuppressor{
		playedSamples:          make([]float64, maxSamples),
		maxSamples:             maxSamples,
		echoThreshold:          0.80, // more conservative for sliding window search
		echoSilenceMS:          2000, // cover longer playback→mic delays
		recentPlaybackWindowMS: 2000,
		enabled:                true,
		playbackSampleRate:     playbackRate,
		inputSampleRate:        inputRate,
	}
}

// RecordPlayedAudio records audio that was just sent to speakers
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

// getSampleAt returns the sample at index i (relative to start of history)
// caller must hold es.mu
func (es *EchoSuppressor) getSampleAt(i int) float64 {
	idx := (es.writeIdx - es.count + i + es.maxSamples) % es.maxSamples
	return es.playedSamples[idx]
}

// IsEcho checks if input audio is primarily echo from speakers
func (es *EchoSuppressor) IsEcho(inputChunk []byte) bool {
	return es.isEchoImpl(inputChunk, false)
}

// IsEchoFast is a faster version of IsEcho that only searches the most recent
// part of the reference buffer (useful for real-time barge-in detection).
func (es *EchoSuppressor) IsEchoFast(inputChunk []byte) bool {
	return es.isEchoImpl(inputChunk, true)
}

func (es *EchoSuppressor) isEchoImpl(inputChunk []byte, fast bool) bool {
	if !es.enabled || len(inputChunk) == 0 {
		return false
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	// If we haven't played audio recently, no echo possible
	if time.Since(es.lastTTSTime) > time.Duration(es.echoSilenceMS)*time.Millisecond {
		return false
	}

	searchSize := es.count
	if fast {
		// 500ms window scaled by playback sample rate
		maxWindow := es.playbackSampleRate / 2
		if searchSize > maxWindow {
			searchSize = maxWindow
		}
	}

	threshold := es.echoThreshold
	inputSamples := bytesToSamples(inputChunk)
	// if the input and playback were recorded at different rates, resample the
	// input to match the playback rate before performing any correlation.
	if es.inputSampleRate != es.playbackSampleRate {
		inputSamples = resample(inputSamples, es.inputSampleRate, es.playbackSampleRate)
	}
	correlation := es.maxCorrelationRing(inputSamples, searchSize)

	// If correlation is high, it's echo
	if correlation > threshold {
		return true
	}

	// Fallback to envelope correlation for 'S' sounds
	envCorr := es.maxEnvelopeCorrelationRing(inputSamples, searchSize, 8)
	return envCorr > threshold+0.05
}

// maxCorrelationRing performing a sliding-window search directly on the ring buffer.
// caller MUST hold es.mu
func (es *EchoSuppressor) maxCorrelationRing(inputSamples []float64, searchSize int) float64 {
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
	stride := compareLen / 8
	if stride < 16 {
		stride = 16
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

// maxEnvelopeCorrelationRing direct ring-buffer version
// caller MUST hold es.mu
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
	stride := compareLen / 4
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

// bytesToSamples converts byte array (16-bit little-endian) to float64 samples in [-1, 1]
func bytesToSamples(data []byte) []float64 {
	samples := make([]float64, 0, len(data)/2)
	for i := 0; i < len(data)-1; i += 2 {
		sample := int16(data[i]) | (int16(data[i+1]) << 8)
		normalized := float64(sample) / 32768.0
		samples = append(samples, normalized)
	}
	return samples
}

// resample performs a simple linear resampling of `samples` from inRate to
// outRate.  It is not high-quality, but sufficient for echo correlation where
// phase alignment is more important than fidelity.  Returns nil if conversion
// would produce zero samples.
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

// calculateEnergy computes the sum of squared samples
func calculateEnergy(samples []float64) float64 {
	energy := 0.0
	for _, s := range samples {
		energy += s * s
	}
	return energy
}

// ClearEchoBuffer clears the played audio buffer
func (es *EchoSuppressor) ClearEchoBuffer() {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.writeIdx = 0
	es.count = 0
}

// PostProcess runs offline echo removal on `input` PCM
func (es *EchoSuppressor) PostProcess(input []byte) []byte {
	if !es.enabled || len(input) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	// frame-based post processing uses the input stream rate so callers feeding
	// 16k audio still get 20ms frames, and we resample each frame before
	// running against the playback reference if the rates differ.
	const frameMs = 20

	es.mu.Lock()
	searchSize := es.count
	threshold := es.echoThreshold
	inputSamples := bytesToSamples(input)
	out := make([]byte, len(input))
	copy(out, input)

	inputRate := es.inputSampleRate
	if inputRate <= 0 {
		inputRate = 44100 // backward compatibility
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

		corr := es.maxCorrelationRing(frame, searchSize)
		if corr > threshold {
			for j := i * 2; j < end*2 && j < len(out); j++ {
				out[j] = 0
			}
		}
	}
	es.mu.Unlock()

	return out
}

// RemoveEchoRealtime attempts to mute echo in real time
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
	threshold := es.echoThreshold

	if searchSize == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	inSamples := bytesToSamples(input)
	if es.inputSampleRate != es.playbackSampleRate {
		inSamples = resample(inSamples, es.inputSampleRate, es.playbackSampleRate)
	}
	maxCorr := es.maxCorrelationRing(inSamples, searchSize)

	if maxCorr < threshold {
		envCorr := es.maxEnvelopeCorrelationRing(inSamples, searchSize, 8)
		if envCorr < threshold+0.05 {
			out := make([]byte, len(input))
			copy(out, input)
			return out
		}
	}

	return make([]byte, len(input))
}

// SetThreshold adjusts the echo detection sensitivity
func (es *EchoSuppressor) SetThreshold(threshold float64) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if threshold >= 0 && threshold <= 1 {
		es.echoThreshold = threshold
	}
}

// SetPlaybackSampleRate updates the rate used for the played-audio buffer. It
// also resizes the internal ring buffer to accommodate roughly two seconds of
// audio at the new rate.  Calling this while audio is buffered will lose the
// old history.
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

// SetInputSampleRate sets the sample rate expected for incoming audio. This
// rate is used for frame sizing and optional resampling to the playback rate.
func (es *EchoSuppressor) SetInputSampleRate(rate int) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if rate <= 0 {
		return
	}
	es.inputSampleRate = rate
}

// SetSampleRates is a convenience wrapper that adjusts both playback and input
// rates together. It is equivalent to calling SetPlaybackSampleRate and
// SetInputSampleRate sequentially.
func (es *EchoSuppressor) SetSampleRates(playbackRate, inputRate int) {
	es.SetPlaybackSampleRate(playbackRate)
	es.SetInputSampleRate(inputRate)
}

// SetEnabled enables or disables echo suppression
func (es *EchoSuppressor) SetEnabled(enabled bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.enabled = enabled
}
