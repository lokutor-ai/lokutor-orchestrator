package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// Conversation is a high-level, user-friendly API for voice conversations
// It wraps the Orchestrator and ConversationSession to provide a simpler interface
// for common voice conversation patterns.
type Conversation struct {
	orch    *Orchestrator
	session *ConversationSession
}

// NewConversation creates a new voice conversation with sensible defaults.
//
// Example:
//
//	conv := orchestrator.NewConversation(stt, llm, tts)
//	conv.SetSystemPrompt("You are a helpful assistant")
//	response, err := conv.Chat(ctx, "Hello!")
func NewConversation(stt STTProvider, llm LLMProvider, tts TTSProvider) *Conversation {
	// Create orchestrator with sensible defaults
	config := DefaultConfig()
	orch := New(stt, llm, tts, config)

	// Create session with unique ID
	session := NewConversationSession("conv_" + fmt.Sprintf("%d", time.Now().UnixNano()))

	return &Conversation{
		orch:    orch,
		session: session,
	}
}

// NewConversationWithConfig creates a new voice conversation with custom configuration.
func NewConversationWithConfig(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config) *Conversation {
	orch := New(stt, llm, tts, config)
	session := NewConversationSession("conv_" + fmt.Sprintf("%d", time.Now().UnixNano()))

	return &Conversation{
		orch:    orch,
		session: session,
	}
}

// SetVoice changes the voice for responses (F1-F5, M1-M5).
//
// Example:
//
//	conv.SetVoice(orchestrator.VoiceM1)
func (c *Conversation) SetVoice(voice Voice) {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentVoice = voice
}

// SetVoiceByString changes the voice using a string (e.g., "M1", "F3").
func (c *Conversation) SetVoiceByString(voice string) error {
	v := Voice(voice)
	// Validate voice
	validVoices := map[Voice]bool{
		VoiceF1: true, VoiceF2: true, VoiceF3: true, VoiceF4: true, VoiceF5: true,
		VoiceM1: true, VoiceM2: true, VoiceM3: true, VoiceM4: true, VoiceM5: true,
	}
	if !validVoices[v] {
		return fmt.Errorf("invalid voice: %s (must be F1-F5 or M1-M5)", voice)
	}
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentVoice = v
	return nil
}

// SetLanguage changes the language for responses.
func (c *Conversation) SetLanguage(language Language) {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentLanguage = language
}

// SetLanguageByString changes the language using a string (e.g., "en", "es").
func (c *Conversation) SetLanguageByString(language string) error {
	lang := Language(language)
	// Validate language
	validLanguages := map[Language]bool{
		LanguageEn: true, LanguageEs: true, LanguageFr: true, LanguageDe: true,
		LanguageIt: true, LanguagePt: true, LanguageJa: true, LanguageZh: true,
	}
	if !validLanguages[lang] {
		return fmt.Errorf("invalid language: %s", language)
	}
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentLanguage = lang
	return nil
}

// SetSystemPrompt adds a system prompt to guide the LLM behavior.
// This is typically called before starting a conversation.
//
// Example:
//
//	conv.SetSystemPrompt("You are a helpful customer service assistant. Be concise and friendly.")
func (c *Conversation) SetSystemPrompt(prompt string) {
	c.session.AddMessage("system", prompt)
}

// ProcessAudio processes audio input: STT -> LLM -> TTS with streaming.
// It transcribes the audio, generates an LLM response, and streams the TTS output.
//
// The onAudioChunk callback is called for each chunk of synthesized audio.
//
// Returns:
//   - transcript: The transcribed text from the audio
//   - response: The LLM-generated response text
//   - error: If any step fails
//
// Example:
//
//	transcript, response, err := conv.ProcessAudio(ctx, audioBytes, func(chunk []byte) error {
//		// Stream audio chunk to user
//		return sendToSpeaker(chunk)
//	})
func (c *Conversation) ProcessAudio(ctx context.Context, audioBytes []byte, onAudioChunk func([]byte) error) (string, string, error) {
	transcript, err := c.orch.ProcessAudioStream(ctx, c.session, audioBytes, onAudioChunk)
	if err != nil {
		return "", "", err
	}

	response := c.session.LastAssistant
	c.orch.logger.Info("audio processed", "sessionID", c.session.ID, "transcriptLen", len(transcript), "responseLen", len(response))

	return transcript, response, nil
}

// Chat sends a text message and gets a voice response (LLM -> TTS streaming).
// The onAudioChunk callback is called for each chunk of synthesized audio.
//
// Example:
//
//	response, err := conv.Chat(ctx, "What's the weather?", func(chunk []byte) error {
//		return sendToSpeaker(chunk)
//	})
func (c *Conversation) Chat(ctx context.Context, text string, onAudioChunk func([]byte) error) (string, error) {
	c.orch.logger.Info("chat message received", "sessionID", c.session.ID, "messageLen", len(text))
	c.session.AddMessage("user", text)

	response, err := c.orch.GenerateResponse(ctx, c.session)
	if err != nil {
		c.orch.logger.Error("chat response generation failed", "sessionID", c.session.ID, "error", err)
		return "", err
	}

	c.session.AddMessage("assistant", response)
	c.orch.logger.Info("chat response generated", "sessionID", c.session.ID, "responseLen", len(response))

	// Stream TTS
	err = c.orch.SynthesizeStream(ctx, response, c.session.CurrentVoice, onAudioChunk)
	if err != nil {
		c.orch.logger.Error("TTS streaming failed in chat", "sessionID", c.session.ID, "error", err)
		return "", err
	}

	return response, nil
}

// TextOnly sends a text message and gets a text response (no TTS).
// Useful for debugging or text-only interactions.
//
// Example:
//
//	response, err := conv.TextOnly(ctx, "What's the capital of France?")
func (c *Conversation) TextOnly(ctx context.Context, text string) (string, error) {
	c.orch.logger.Info("text-only message received", "sessionID", c.session.ID, "messageLen", len(text))
	c.session.AddMessage("user", text)

	response, err := c.orch.GenerateResponse(ctx, c.session)
	if err != nil {
		c.orch.logger.Error("text-only response generation failed", "sessionID", c.session.ID, "error", err)
		return "", err
	}

	c.session.AddMessage("assistant", response)
	c.orch.logger.Info("text-only response generated", "sessionID", c.session.ID, "responseLen", len(response))

	return response, nil
}

// GetContext returns the full conversation history as a slice of messages.
func (c *Conversation) GetContext() []Message {
	return c.session.GetContextCopy()
}

// GetLastUserMessage returns the user's last message.
func (c *Conversation) GetLastUserMessage() string {
	c.session.mu.RLock()
	defer c.session.mu.RUnlock()
	return c.session.LastUser
}

// GetLastAssistantMessage returns the assistant's last message.
func (c *Conversation) GetLastAssistantMessage() string {
	c.session.mu.RLock()
	defer c.session.mu.RUnlock()
	return c.session.LastAssistant
}

// ClearContext resets conversation history but keeps system prompt and settings.
// Useful for starting a new conversation topic while keeping instructions.
//
// Example:
//
//	conv.ClearContext() // Keep system prompt, reset conversation
func (c *Conversation) ClearContext() {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	// Keep system messages
	system := []Message{}
	for _, msg := range c.session.Context {
		if msg.Role == "system" {
			system = append(system, msg)
		}
	}
	c.session.Context = system
	c.session.LastUser = ""
	c.session.LastAssistant = ""
}

// Reset clears everything including system prompts and settings.
// Returns the conversation to a fresh state.
func (c *Conversation) Reset() {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.Context = []Message{}
	c.session.LastUser = ""
	c.session.LastAssistant = ""
	c.session.CurrentVoice = VoiceF1
	c.session.CurrentLanguage = LanguageEn
}

// GetSessionID returns the unique session ID for this conversation.
func (c *Conversation) GetSessionID() string {
	return c.session.ID
}

// GetProviders returns information about the current providers (STT, LLM, TTS).
func (c *Conversation) GetProviders() map[string]string {
	return c.orch.GetProviders()
}

// GetConfig returns the current orchestrator configuration.
func (c *Conversation) GetConfig() Config {
	return c.orch.GetConfig()
}
