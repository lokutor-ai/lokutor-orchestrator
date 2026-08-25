package orchestrator

import (
	"context"
	"testing"
)

func TestConversation(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}

	conv := NewConversation(stt, llm, tts)

	t.Run("NewConversationWithConfig", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxContextMessages = 5
		conv2 := NewConversationWithConfig(stt, llm, tts, config)
		if conv2.GetConfig().MaxContextMessages != 5 {
			t.Errorf("expected 5, got %d", conv2.GetConfig().MaxContextMessages)
		}
	})

	t.Run("SetVoice", func(t *testing.T) {
		conv.SetVoice(VoiceM1)
		if conv.session.GetCurrentVoice() != VoiceM1 {
			t.Errorf("expected VoiceM1, got %v", conv.session.GetCurrentVoice())
		}
	})

	t.Run("SetVoiceByString", func(t *testing.T) {
		err := conv.SetVoiceByString("F2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conv.session.GetCurrentVoice() != VoiceF2 {
			t.Errorf("expected VoiceF2, got %v", conv.session.GetCurrentVoice())
		}

		err = conv.SetVoiceByString("invalid")
		if err == nil {
			t.Error("expected error for invalid voice")
		}
	})

	t.Run("SetLanguage", func(t *testing.T) {
		conv.SetLanguage(LanguageEs)
		if conv.session.GetCurrentLanguage() != LanguageEs {
			t.Errorf("expected LanguageEs, got %v", conv.session.GetCurrentLanguage())
		}
	})

	t.Run("SetLanguageByString", func(t *testing.T) {
		err := conv.SetLanguageByString("fr")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conv.session.GetCurrentLanguage() != LanguageFr {
			t.Errorf("expected LanguageFr, got %v", conv.session.GetCurrentLanguage())
		}

		err = conv.SetLanguageByString("invalid")
		if err == nil {
			t.Error("expected error for invalid language")
		}
	})

	t.Run("SetSystemPrompt", func(t *testing.T) {
		conv.SetSystemPrompt("test prompt")
		ctx := conv.GetContext()
		found := false
		for _, m := range ctx {
			if m.Role == "system" && m.Content == "test prompt" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected system prompt to be in context")
		}
	})

	t.Run("Chat", func(t *testing.T) {
		resp, err := conv.Chat(context.Background(), "hi", func(chunk []byte) error {
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp != "world" {
			t.Errorf("expected 'world', got '%s'", resp)
		}
	})

	t.Run("ProcessAudio", func(t *testing.T) {
		transcript, response, err := conv.ProcessAudio(context.Background(), []byte{1, 2, 3}, func(chunk []byte) error {
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if transcript != "hello" {
			t.Errorf("expected 'hello', got '%s'", transcript)
		}
		if response != "world" {
			t.Errorf("expected 'world', got '%s'", response)
		}
	})

	t.Run("TextOnly", func(t *testing.T) {
		resp, err := conv.TextOnly(context.Background(), "hi text")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp != "world" {
			t.Errorf("expected 'world', got '%s'", resp)
		}
	})

	t.Run("ClearContext", func(t *testing.T) {
		conv.ClearContext()
		conv.session.mu.RLock()

		if len(conv.session.Context) != 1 {
			t.Errorf("expected 1 message (system prompt), got %d", len(conv.session.Context))
		}
		conv.session.mu.RUnlock()
	})

	t.Run("Reset", func(t *testing.T) {
		conv.Reset()
		conv.session.mu.RLock()
		if len(conv.session.Context) != 0 {
			t.Errorf("expected 0 messages after reset, got %d", len(conv.session.Context))
		}
		conv.session.mu.RUnlock()
	})

	t.Run("Getters", func(t *testing.T) {
		conv.Chat(context.Background(), "hello", func(chunk []byte) error { return nil })
		if conv.GetSessionID() == "" {
			t.Error("expected non-empty session ID")
		}
		if conv.GetLastUserMessage() == "" {
			t.Error("expected last user message")
		}
		if conv.GetLastAssistantMessage() == "" {
			t.Error("expected last assistant message")
		}
		providers := conv.GetProviders()
		if providers["llm"] != "MockLLM" {
			t.Errorf("expected 'MockLLM', got '%s'", providers["llm"])
		}
		if conv.GetConfig().SampleRate == 0 {
			t.Error("expected non-zero sample rate")
		}
	})
}
