package noise

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// STTWrapper wraps an STT provider with noise suppression.
// It resamples audio to 16kHz for the filter, then back to the original rate for STT.
type STTWrapper struct {
	inner      orchestrator.STTProvider
	filter     *Filter
	sampleRate int
}

// NewSTTWrapper creates a noise-suppressing wrapper around an STT provider.
func NewSTTWrapper(inner orchestrator.STTProvider, modelPath string, sampleRate int) (*STTWrapper, error) {
	filter, err := NewFilter(modelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create noise filter: %w", err)
	}
	return &STTWrapper{
		inner:      inner,
		filter:     filter,
		sampleRate: sampleRate,
	}, nil
}

// Name returns the wrapped provider name with noise filter prefix.
func (w *STTWrapper) Name() string {
	return w.inner.Name() + "+noise-filter"
}

// Transcribe applies noise suppression before transcribing.
func (w *STTWrapper) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	// Convert bytes to float32 samples (16-bit PCM)
	samples := pcmBytesToFloat32(audioPCM)

	// Resample to 16kHz if needed
	if w.sampleRate != SampleRate {
		samples = ResampleLinear(samples, w.sampleRate, SampleRate)
	}

	// Apply noise suppression at 16kHz
	cleanSamples := w.filter.ProcessChunk(samples)
	cleanSamples = append(cleanSamples, w.filter.Flush()...)

	// Resample back to original rate if needed
	if w.sampleRate != SampleRate {
		cleanSamples = ResampleLinear(cleanSamples, SampleRate, w.sampleRate)
	}

	// Convert back to bytes
	cleanPCM := float32ToPCMBytes(cleanSamples)

	// Transcribe clean audio
	return w.inner.Transcribe(ctx, cleanPCM, lang)
}

// Destroy cleans up the noise filter.
func (w *STTWrapper) Destroy() {
	if w.filter != nil {
		w.filter.Destroy()
	}
}

func pcmBytesToFloat32(data []byte) []float32 {
	nSamples := len(data) / 2
	samples := make([]float32, nSamples)
	for i := 0; i < nSamples; i++ {
		val := int16(binary.LittleEndian.Uint16(data[i*2:]))
		samples[i] = float32(val) / 32768.0
	}
	return samples
}

func float32ToPCMBytes(samples []float32) []byte {
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		val := int16(s * 32767.0)
		binary.LittleEndian.PutUint16(data[i*2:], uint16(val))
	}
	return data
}
