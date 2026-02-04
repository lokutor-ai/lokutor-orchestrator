package orchestrator

import (
	"context"
	"testing"
)

// MockSTTProvider for testing
type MockSTTProvider struct {
	transcribeResult string
	transcribeErr    error
}

func (m *MockSTTProvider) Transcribe(ctx context.Context, audio []byte) (string, error) {
	return m.transcribeResult, m.transcribeErr
}

func (m *MockSTTProvider) Name() string {
	return "MockSTT"
}

// MockLLMProvider for testing
type MockLLMProvider struct {
	completeResult string
	completeErr    error
}

func (m *MockLLMProvider) Complete(ctx context.Context, messages []Message) (string, error) {
	return m.completeResult, m.completeErr
}

func (m *MockLLMProvider) Name() string {
	return "MockLLM"
}

// MockTTSProvider for testing
type MockTTSProvider struct {
	synthesizeResult []byte
	synthesizeErr    error
	streamErr        error
}

func (m *MockTTSProvider) Synthesize(ctx context.Context, text string, voice Voice) ([]byte, error) {
	return m.synthesizeResult, m.synthesizeErr
}

func (m *MockTTSProvider) StreamSynthesize(ctx context.Context, text string, voice Voice, onChunk func([]byte) error) error {
	if m.streamErr != nil {
		return m.streamErr
	}
	return onChunk(m.synthesizeResult)
}

func (m *MockTTSProvider) Name() string {
	return "MockTTS"
}

// Test orchestrator creation
func TestOrchestratorCreation(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{}
	config := DefaultConfig()

	orch := New(stt, llm, tts, config)

	if orch == nil {
		t.Fatal("Expected orchestrator to be created")
	}

	providers := orch.GetProviders()
	if providers["stt"] != "MockSTT" {
		t.Errorf("Expected STT provider name to be 'MockSTT', got %s", providers["stt"])
	}
	if providers["llm"] != "MockLLM" {
		t.Errorf("Expected LLM provider name to be 'MockLLM', got %s", providers["llm"])
	}
	if providers["tts"] != "MockTTS" {
		t.Errorf("Expected TTS provider name to be 'MockTTS', got %s", providers["tts"])
	}
}

// Test ProcessAudio full pipeline
func TestProcessAudio(t *testing.T) {
	stt := &MockSTTProvider{
		transcribeResult: "Hello, how are you?",
	}
	llm := &MockLLMProvider{
		completeResult: "I'm doing great, thanks for asking!",
	}
	tts := &MockTTSProvider{
		synthesizeResult: []byte{0x01, 0x02, 0x03, 0x04},
	}

	orch := New(stt, llm, tts, DefaultConfig())
	session := NewConversationSession("test_user")

	transcript, audioBytes, err := orch.ProcessAudio(
		context.Background(),
		session,
		[]byte{0xFF, 0xFE},
	)

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if transcript != "Hello, how are you?" {
		t.Errorf("Expected transcript 'Hello, how are you?', got '%s'", transcript)
	}

	if len(audioBytes) != 4 {
		t.Errorf("Expected 4 audio bytes, got %d", len(audioBytes))
	}

	if len(session.Context) != 2 {
		t.Errorf("Expected 2 messages in context, got %d", len(session.Context))
	}

	if session.Context[0].Role != "user" {
		t.Errorf("Expected first message role to be 'user', got '%s'", session.Context[0].Role)
	}

	if session.Context[1].Role != "assistant" {
		t.Errorf("Expected second message role to be 'assistant', got '%s'", session.Context[1].Role)
	}
}

// Test streaming TTS
func TestProcessAudioStream(t *testing.T) {
	stt := &MockSTTProvider{
		transcribeResult: "Hello",
	}
	llm := &MockLLMProvider{
		completeResult: "Hi there!",
	}
	tts := &MockTTSProvider{
		synthesizeResult: []byte{0x01, 0x02},
	}

	orch := New(stt, llm, tts, DefaultConfig())
	session := NewConversationSession("test_user")

	chunks := [][]byte{}
	transcript, err := orch.ProcessAudioStream(
		context.Background(),
		session,
		[]byte{0xFF, 0xFE},
		func(chunk []byte) error {
			chunks = append(chunks, chunk)
			return nil
		},
	)

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if transcript != "Hello" {
		t.Errorf("Expected transcript 'Hello', got '%s'", transcript)
	}

	if len(chunks) == 0 {
		t.Fatal("Expected at least one audio chunk")
	}
}

// Test configuration management
func TestConfigManagement(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{}

	originalConfig := DefaultConfig()
	orch := New(stt, llm, tts, originalConfig)

	cfg := orch.GetConfig()
	if cfg.SampleRate != originalConfig.SampleRate {
		t.Errorf("Expected sample rate %d, got %d", originalConfig.SampleRate, cfg.SampleRate)
	}

	newConfig := Config{
		SampleRate:         8000,
		Channels:           1,
		BytesPerSamp:       2,
		MaxContextMessages: 50,
		VoiceStyle:         VoiceM1,
		Language:           LanguageEs,
	}
	orch.UpdateConfig(newConfig)

	updatedCfg := orch.GetConfig()
	if updatedCfg.SampleRate != 8000 {
		t.Errorf("Expected updated sample rate 8000, got %d", updatedCfg.SampleRate)
	}
	if updatedCfg.VoiceStyle != VoiceM1 {
		t.Errorf("Expected voice M1, got %s", updatedCfg.VoiceStyle)
	}
}

// Test interruption handling
func TestHandleInterruption(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{}

	orch := New(stt, llm, tts, DefaultConfig())
	session := NewConversationSession("test_user")

	orch.HandleInterruption(session)
}

// TestConcurrentSessionOperations tests that multiple goroutines can safely access a session
func TestConcurrentSessionOperations(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "Hello"}
	llm := &MockLLMProvider{completeResult: "Hi there"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}

	orch := New(stt, llm, tts, DefaultConfig())
	session := NewConversationSession("concurrent_test")

	// Run multiple operations concurrently
	numGoroutines := 10
	done := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			_, _, err := orch.ProcessAudio(context.Background(), session, []byte("audio"))
			if err != nil {
				t.Errorf("ProcessAudio failed: %v", err)
			}
			done <- true
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify session integrity
	if len(session.Context) == 0 {
		t.Fatal("session context should not be empty after concurrent operations")
	}
}

// TestConfigThreadSafety tests that config can be read and updated safely
func TestConfigThreadSafety(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{}

	config := DefaultConfig()
	orch := New(stt, llm, tts, config)

	done := make(chan bool, 20)

	// 10 goroutines writing config
	for i := 0; i < 10; i++ {
		go func(val int) {
			cfg := orch.GetConfig()
			cfg.MaxContextMessages = val
			orch.UpdateConfig(cfg)
			done <- true
		}(i)
	}

	// 10 goroutines reading config
	for i := 0; i < 10; i++ {
		go func() {
			_ = orch.GetConfig()
			done <- true
		}()
	}

	// Wait for all to complete
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify config is still valid
	cfg := orch.GetConfig()
	if cfg.SampleRate == 0 {
		t.Fatal("config was corrupted")
	}
}

// TestContextCancellation tests that operations respect context cancellation
func TestContextCancellation(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "Hello", transcribeErr: context.Canceled}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{}

	orch := New(stt, llm, tts, DefaultConfig())
	session := NewConversationSession("cancel_test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, _, err := orch.ProcessAudio(ctx, session, []byte("audio"))
	if err == nil {
		t.Fatal("ProcessAudio should return error when context is cancelled")
	}
}

// TestCustomErrorTypes tests that proper custom error types are returned
func TestCustomErrorTypes(t *testing.T) {
	tests := []struct {
		name        string
		stt         STTProvider
		llm         LLMProvider
		tts         TTSProvider
		expectedErr error
	}{
		{
			name:        "empty transcription",
			stt:         &MockSTTProvider{transcribeResult: "   "},
			llm:         &MockLLMProvider{completeResult: "response"},
			tts:         &MockTTSProvider{synthesizeResult: []byte("audio")},
			expectedErr: ErrEmptyTranscription,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orch := New(tt.stt, tt.llm, tt.tts, DefaultConfig())
			session := NewConversationSession("error_test")

			_, _, err := orch.ProcessAudio(context.Background(), session, []byte("audio"))
			if !isErrorType(err, tt.expectedErr) {
				t.Errorf("expected error type %T, got %T: %v", tt.expectedErr, err, err)
			}
		})
	}
}

// Helper function to check if an error is of a specific type
func isErrorType(err error, target error) bool {
	if err == nil && target == nil {
		return true
	}
	if err == nil || target == nil {
		return false
	}
	return err.Error() == target.Error()
}
