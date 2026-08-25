package orchestrator

import (
	"context"
	"errors"
)

var ErrTestError = errors.New("test error")

// MockSTTProvider is a mock STT provider for tests.
type MockSTTProvider struct {
	transcribeResult string
	transcribeErr    error
}

func (m *MockSTTProvider) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	return TranscriptionResult{Text: m.transcribeResult}, m.transcribeErr
}

func (m *MockSTTProvider) Name() string { return "MockSTT" }

// MockLLMProvider is a mock LLM provider for tests.
type MockLLMProvider struct {
	completeResult string
	completeErr    error
}

func (m *MockLLMProvider) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return m.completeResult, m.completeErr
}

func (m *MockLLMProvider) Name() string { return "MockLLM" }

// MockTTSProvider is a mock TTS provider for tests.
type MockTTSProvider struct {
	synthesizeResult []byte
	synthesizeErr    error
	streamErr        error
}

func (m *MockTTSProvider) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return m.synthesizeResult, m.synthesizeErr
}

func (m *MockTTSProvider) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	if m.streamErr != nil {
		return m.streamErr
	}
	return onChunk(m.synthesizeResult)
}

func (m *MockTTSProvider) Abort() error { return nil }
func (m *MockTTSProvider) Name() string { return "MockTTS" }
