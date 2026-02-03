package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// Orchestrator manages the STT -> LLM -> TTS pipeline
type Orchestrator struct {
	stt    STTProvider
	llm    LLMProvider
	tts    TTSProvider
	config Config
	mu     sync.RWMutex
}

// New creates a new orchestrator with the given providers
func New(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config) *Orchestrator {
	return &Orchestrator{
		stt:    stt,
		llm:    llm,
		tts:    tts,
		config: config,
	}
}

// ProcessAudio handles the full pipeline: STT -> LLM -> TTS
func (o *Orchestrator) ProcessAudio(ctx context.Context, session *ConversationSession, audioData []byte) (string, []byte, error) {
	// 1. Transcribe audio
	transcript, err := o.Transcribe(ctx, audioData)
	if err != nil {
		return "", nil, fmt.Errorf("transcription failed: %w", err)
	}

	if strings.TrimSpace(transcript) == "" {
		return "", nil, fmt.Errorf("empty transcription")
	}

	log.Printf("[%s] Transcribed: %s", session.ID, transcript)
	session.AddMessage("user", transcript)

	// 2. Get LLM response
	response, err := o.GenerateResponse(ctx, session)
	if err != nil {
		return transcript, nil, fmt.Errorf("LLM failed: %w", err)
	}

	log.Printf("[%s] Response: %s", session.ID, response)
	session.AddMessage("assistant", response)

	// 3. Synthesize response to audio
	audioBytes, err := o.Synthesize(ctx, response, session.CurrentVoice)
	if err != nil {
		return transcript, nil, fmt.Errorf("TTS failed: %w", err)
	}

	return transcript, audioBytes, nil
}

// ProcessAudioStream handles streaming TTS output
func (o *Orchestrator) ProcessAudioStream(ctx context.Context, session *ConversationSession, audioData []byte, onAudioChunk func([]byte) error) (string, error) {
	// 1. Transcribe
	transcript, err := o.Transcribe(ctx, audioData)
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}

	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("empty transcription")
	}

	log.Printf("[%s] Transcribed: %s", session.ID, transcript)
	session.AddMessage("user", transcript)

	// 2. Get LLM response
	response, err := o.GenerateResponse(ctx, session)
	if err != nil {
		return transcript, fmt.Errorf("LLM failed: %w", err)
	}

	log.Printf("[%s] Response: %s", session.ID, response)
	session.AddMessage("assistant", response)

	// 3. Stream TTS output
	err = o.SynthesizeStream(ctx, response, session.CurrentVoice, onAudioChunk)
	if err != nil {
		return transcript, fmt.Errorf("TTS stream failed: %w", err)
	}

	return transcript, nil
}

// Transcribe converts audio to text
func (o *Orchestrator) Transcribe(ctx context.Context, audioData []byte) (string, error) {
	return o.stt.Transcribe(ctx, audioData)
}

// GenerateResponse gets an LLM response based on session context
func (o *Orchestrator) GenerateResponse(ctx context.Context, session *ConversationSession) (string, error) {
	return o.llm.Complete(ctx, session.Context)
}

// Synthesize converts text to speech audio
func (o *Orchestrator) Synthesize(ctx context.Context, text string, voice Voice) ([]byte, error) {
	return o.tts.Synthesize(ctx, text, voice)
}

// SynthesizeStream converts text to speech with streaming
func (o *Orchestrator) SynthesizeStream(ctx context.Context, text string, voice Voice, onChunk func([]byte) error) error {
	return o.tts.StreamSynthesize(ctx, text, voice, onChunk)
}

// HandleInterruption processes an interruption in the conversation
func (o *Orchestrator) HandleInterruption(session *ConversationSession) {
	log.Printf("[%s] Conversation interrupted", session.ID)
	// Can be extended for custom interruption handling
}

// UpdateConfig updates the orchestrator configuration
func (o *Orchestrator) UpdateConfig(cfg Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.config = cfg
}

// GetConfig returns the current configuration
func (o *Orchestrator) GetConfig() Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.config
}

// GetProviders returns information about the current providers
func (o *Orchestrator) GetProviders() map[string]string {
	return map[string]string{
		"stt": o.stt.Name(),
		"llm": o.llm.Name(),
		"tts": o.tts.Name(),
	}
}

// NewSessionWithDefaults creates a new conversation session with orchestrator's default config
// This automatically applies the orchestrator's configured defaults to the session
func (o *Orchestrator) NewSessionWithDefaults(userID string) *ConversationSession {
	session := NewConversationSession(userID)
	session.MaxMessages = o.config.MaxContextMessages
	session.CurrentVoice = o.config.VoiceStyle
	session.CurrentLanguage = o.config.Language
	return session
}

// SetSystemPrompt adds a system prompt message to a session
// This is a convenience method - equivalent to session.AddMessage("system", prompt)
func (o *Orchestrator) SetSystemPrompt(session *ConversationSession, prompt string) {
	session.AddMessage("system", prompt)
}

// SetVoice changes the voice for a session
// This is a convenience method - equivalent to setting session.CurrentVoice directly
func (o *Orchestrator) SetVoice(session *ConversationSession, voice Voice) {
	session.CurrentVoice = voice
}

// SetLanguage changes the language for a session
// This is a convenience method - equivalent to setting session.CurrentLanguage directly
func (o *Orchestrator) SetLanguage(session *ConversationSession, lang Language) {
	session.CurrentLanguage = lang
}

// ResetSession clears the conversation history for a session
// This is a convenience method - equivalent to session.ClearContext()
func (o *Orchestrator) ResetSession(session *ConversationSession) {
	session.ClearContext()
}
