package orchestrator

import "errors"

// Custom error types for better error discrimination
var (
	// ErrEmptyTranscription is returned when transcription produces empty text
	ErrEmptyTranscription = errors.New("transcription returned empty text")

	// ErrTranscriptionFailed is returned when STT provider fails
	ErrTranscriptionFailed = errors.New("speech-to-text transcription failed")

	// ErrLLMFailed is returned when LLM provider fails
	ErrLLMFailed = errors.New("language model generation failed")

	// ErrTTSFailed is returned when TTS provider fails
	ErrTTSFailed = errors.New("text-to-speech synthesis failed")

	// ErrNilProvider is returned when a required provider is nil
	ErrNilProvider = errors.New("required provider is nil")

	// ErrContextCancelled is returned when operation is cancelled via context
	ErrContextCancelled = errors.New("operation cancelled by context")
)
