package orchestrator

import (
	"context"
	"fmt"
	"time"
)

type Conversation struct {
	orch    *Orchestrator
	session *ConversationSession
}

func NewConversation(stt STTProvider, llm LLMProvider, tts TTSProvider) *Conversation {

	config := DefaultConfig()
	orch := New(stt, llm, tts, nil, config, nil)

	session := NewConversationSession("conv_" + fmt.Sprintf("%d", time.Now().UnixNano()))

	return &Conversation{
		orch:    orch,
		session: session,
	}
}

func NewConversationWithConfig(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config) *Conversation {
	orch := New(stt, llm, tts, nil, config, nil)
	session := NewConversationSession("conv_" + fmt.Sprintf("%d", time.Now().UnixNano()))

	return &Conversation{
		orch:    orch,
		session: session,
	}
}

func (c *Conversation) SetVoice(voice Voice) {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentVoice = voice
}

func (c *Conversation) SetVoiceByString(voice string) error {
	v := Voice(voice)

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

func (c *Conversation) SetLanguage(language Language) {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.CurrentLanguage = language
}

func (c *Conversation) SetLanguageByString(language string) error {
	lang := Language(language)

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

func (c *Conversation) SetSystemPrompt(prompt string) {
	c.session.AddMessage("system", prompt)
}

func (c *Conversation) ProcessAudio(ctx context.Context, audioBytes []byte, onAudioChunk func([]byte) error) (string, string, error) {
	transcript, err := c.orch.ProcessAudioStream(ctx, c.session, audioBytes, onAudioChunk)
	if err != nil {
		return "", "", err
	}

	response := c.session.LastAssistant
	c.orch.logger.Info("audio processed", "sessionID", c.session.ID, "transcriptLen", len(transcript), "responseLen", len(response))

	return transcript, response, nil
}

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

	err = c.orch.SynthesizeStream(ctx, response, c.session.CurrentVoice, c.session.CurrentLanguage, onAudioChunk)
	if err != nil {
		c.orch.logger.Error("TTS streaming failed in chat", "sessionID", c.session.ID, "error", err)
		return "", err
	}

	return response, nil
}

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

func (c *Conversation) GetContext() []Message {
	return c.session.GetContextCopy()
}

func (c *Conversation) GetLastUserMessage() string {
	c.session.mu.RLock()
	defer c.session.mu.RUnlock()
	return c.session.LastUser
}

func (c *Conversation) GetLastAssistantMessage() string {
	c.session.mu.RLock()
	defer c.session.mu.RUnlock()
	return c.session.LastAssistant
}

func (c *Conversation) ClearContext() {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()

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

func (c *Conversation) Reset() {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	c.session.Context = []Message{}
	c.session.LastUser = ""
	c.session.LastAssistant = ""
	c.session.CurrentVoice = VoiceF1
	c.session.CurrentLanguage = LanguageEn
}

func (c *Conversation) GetSessionID() string {
	return c.session.ID
}

func (c *Conversation) GetProviders() map[string]string {
	return c.orch.GetProviders()
}

func (c *Conversation) GetConfig() Config {
	return c.orch.GetConfig()
}
