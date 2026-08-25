package orchestrator

import "errors"

var (
	ErrEmptyTranscription = errors.New("transcription returned empty text")

	ErrTranscriptionFailed = errors.New("speech-to-text transcription failed")

	ErrLLMFailed = errors.New("language model generation failed")

	ErrTTSFailed = errors.New("text-to-speech synthesis failed")

	ErrNilProvider = errors.New("required provider is nil")

	ErrContextCancelled = errors.New("operation cancelled by context")
)
