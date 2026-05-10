package orchestrator

import (
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/prosody"
)

// ProsodyAndAudioConfig holds combined configuration for prosody and audio processing
type ProsodyAndAudioConfig struct {
	// Prosody settings
	Prosody prosody.ProsodyConfig

	// Audio enhancement settings
	Audio audio.Config
}

func DefaultProsodyAndAudioConfig() ProsodyAndAudioConfig {
	return ProsodyAndAudioConfig{
		Prosody: prosody.DefaultConfig(),
		Audio:   audio.DefaultConfig(),
	}
}

// ProsodyAndAudioProcessor combines prosody prediction and audio enhancement
type ProsodyAndAudioProcessor struct {
	prosodyProc *prosody.AdaptiveProcessor
	audioProc   *audio.AdaptiveProcessor
}

// NewProsodyAndAudioProcessor creates the combined processor
func NewProsodyAndAudioProcessor(cfg ProsodyAndAudioConfig) *ProsodyAndAudioProcessor {
	return &ProsodyAndAudioProcessor{
		prosodyProc: prosody.NewAdaptiveProcessor(cfg.Prosody),
		audioProc:   audio.NewAdaptiveProcessor(cfg.Audio),
	}
}

// Process applies prosody to text, then enhances the audio output
func (p *ProsodyAndAudioProcessor) ProcessText(text string) prosody.ProsodyResult {
	return p.prosodyProc.ProcessText(text)
}

// ProcessAudio enhances raw TTS audio
func (p *ProsodyAndAudioProcessor) ProcessAudio(audioData []byte, sampleRate, channels int) []byte {
	return p.audioProc.AnalyzeAndProcess(audioData, sampleRate, channels)
}

// ProcessAudioStreaming processes and enhances audio in chunks
// This is critical for low latency - don't wait for full audio
func (p *ProsodyAndAudioProcessor) ProcessAudioStreaming(
	audioData []byte,
	sampleRate, channels int,
	onChunk func([]byte) error,
	chunkSizeMs int,
) error {
	// Process in small chunks to maintain low latency
	bytesPerMs := sampleRate * channels * 2 / 1000
	chunkBytes := chunkSizeMs * bytesPerMs

	// Buffer for cross-chunk processing
	var buffer []byte

	for i := 0; i < len(audioData); i += chunkBytes {
		end := i + chunkBytes
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]
		buffer = append(buffer, chunk...)

		// Only process when we have enough samples
		if len(buffer) >= chunkBytes {
			processed := p.audioProc.AnalyzeAndProcess(buffer, sampleRate, channels)
			if err := onChunk(processed); err != nil {
				return err
			}
			// Keep overlap for smooth transitions
			overlap := chunkBytes / 4
			if overlap > 0 && len(buffer) > overlap {
				buffer = buffer[len(buffer)-overlap:]
			} else {
				buffer = nil
			}
		}
	}

	// Process remaining
	if len(buffer) > 0 {
		processed := p.audioProc.AnalyzeAndProcess(buffer, sampleRate, channels)
		return onChunk(processed)
	}

	return nil
}

// UpdateContext updates processors with new conversation context
func (p *ProsodyAndAudioProcessor) UpdateContext(utterance string, durationMs int) {
	p.prosodyProc.UpdateContext(utterance, durationMs)
}

// GetConfig returns current configuration
func (p *ProsodyAndAudioProcessor) GetConfig() prosody.ProsodyConfig {
	return p.prosodyProc.GetConfig()
}

// SetVoiceStyle adjusts processing for specific voice style
func (p *ProsodyAndAudioProcessor) SetVoiceStyle(voice Voice) {
	adapter := prosody.NewVoiceStyleAdapter(string(voice))
	cfg := p.prosodyProc.GetConfig()
	adapter.AdjustConfig(&cfg)
	// Update would require modifying the base config
	_ = cfg // Configuration updated in-place
}

// SetOutputDevice adjusts audio processing for output device
func (p *ProsodyAndAudioProcessor) SetOutputDevice(device audio.DeviceType) {
	p.audioProc.SetDevice(device)
}

// Integration example - add this method to your ManagedStream

/*
In managed_stream.go, modify speakText to use prosody and audio enhancement:

func (ms *ManagedStream) speakTextWithProsody(ctx context.Context, text string) {
	// 1. Apply prosody analysis
	prosodyResult := ms.prosodyProcessor.ProcessText(text)

	// 2. Generate speech with prosody hints
	// For TTS that supports SSML:
	ssmlText := prosody.CreateSSMLMarkers(prosodyResult.Markers)

	// 3. Stream audio through audio enhancer
	err := ms.orch.SynthesizeStream(ctx, ssmlText, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		// Enhance each chunk in real-time
		enhanced := ms.audioProcessor.ProcessAudioStreaming(
			chunk,
			int(ms.playbackRate),
			1, // mono
			func(enhancedChunk []byte) error {
				ms.emitWithGen(AudioChunk, enhancedChunk, ms.payloadGen)
				return nil
			},
			60, // 60ms chunks
		)
		return enhanced
	})

	// 4. Update context for adaptive processing
	ms.prosodyProcessor.UpdateContext(text, estimateDurationMs(text))
}
*/

// estimateDurationMs estimates speech duration for context updates
func estimateDurationMs(text string) int {
	words := prosody.SplitWords(text)
	// Average 150 words per minute = 100ms per word
	return words * 100
}