package noise

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// STTWrapper wraps an STT provider with noise suppression.
type STTWrapper struct {
	inner  orchestrator.STTProvider
	filter *Filter
}

// NewSTTWrapper creates a noise-suppressing wrapper around an STT provider.
func NewSTTWrapper(inner orchestrator.STTProvider, modelPath string) (*STTWrapper, error) {
	filter, err := NewFilter(modelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create noise filter: %w", err)
	}
	return &STTWrapper{
		inner:  inner,
		filter: filter,
	}, nil
}

// Name returns the wrapped provider name with noise filter prefix.
func (w *STTWrapper) Name() string {
	return w.inner.Name() + "+noise-filter"
}

// Transcribe applies noise suppression before transcribing.
func (w *STTWrapper) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	// Convert bytes to float32 samples (assuming 16-bit PCM)
	samples := pcmBytesToFloat32(audioPCM)

	// Apply noise suppression
	cleanSamples := w.filter.ProcessChunk(samples)
	cleanSamples = append(cleanSamples, w.filter.Flush()...)

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
	// Assume 16-bit little-endian PCM
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
		// Clamp
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

// RMS calculates root-mean-square of a float32 slice.
func RMS(samples []float32) float32 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return float32(math.Sqrt(sum / float64(len(samples))))
}
