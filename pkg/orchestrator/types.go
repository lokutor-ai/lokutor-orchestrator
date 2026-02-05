package orchestrator

import (
	"context"
	"sync"
)

// Logger interface allows custom logging implementations
// Implementations can use structured logging, slog, logrus, zap, or any custom logger
type Logger interface {
	// Debug logs a debug message
	Debug(msg string, args ...interface{})
	// Info logs an info message
	Info(msg string, args ...interface{})
	// Warn logs a warning message
	Warn(msg string, args ...interface{})
	// Error logs an error message
	Error(msg string, args ...interface{})
}

// NoOpLogger is a Logger that discards all messages
type NoOpLogger struct{}

func (n *NoOpLogger) Debug(msg string, args ...interface{}) {}
func (n *NoOpLogger) Info(msg string, args ...interface{})  {}
func (n *NoOpLogger) Warn(msg string, args ...interface{})  {}
func (n *NoOpLogger) Error(msg string, args ...interface{}) {}

// STTProvider is the interface for Speech-to-Text implementations
type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte, lang Language) (string, error)
	Name() string
}

// StreamingSTTProvider allows for real-time transcription
type StreamingSTTProvider interface {
	STTProvider
	StreamTranscribe(ctx context.Context, lang Language, onTranscript func(transcript string, isFinal bool) error) (chan<- []byte, error)
}

// LLMProvider is the interface for Language Model implementations
type LLMProvider interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	Name() string
}

// TTSProvider is the interface for Text-to-Speech implementations
type TTSProvider interface {
	Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error)
	StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error
	Name() string
}

// VADProvider is the interface for Voice Activity Detection implementations
type VADProvider interface {
	Process(chunk []byte) (*VADEvent, error)
	Reset()
	Clone() VADProvider
	Name() string
}

// VADEventType represents the type of VAD event
type VADEventType string

const (
	VADSpeechStart VADEventType = "SPEECH_START"
	VADSpeechEnd   VADEventType = "SPEECH_END"
	VADSilence     VADEventType = "SILENCE"
)

// VADEvent contains details about a VAD detection
type VADEvent struct {
	Type      VADEventType
	Timestamp int64
}

// EventType represents the type of event in the managed stream
type EventType string

const (
	UserSpeaking      EventType = "USER_SPEAKING"
	UserStopped       EventType = "USER_STOPPED"
	TranscriptPartial EventType = "TRANSCRIPT_PARTIAL"
	TranscriptFinal   EventType = "TRANSCRIPT_FINAL"
	BotThinking       EventType = "BOT_THINKING"
	BotSpeaking       EventType = "BOT_SPEAKING"
	Interrupted       EventType = "INTERRUPTED"
	AudioChunk        EventType = "AUDIO_CHUNK"
	ErrorEvent        EventType = "ERROR"
)

// OrchestratorEvent represents a message emitted by the managed stream
type OrchestratorEvent struct {
	Type      EventType   `json:"type"`
	SessionID string      `json:"session_id"`
	Data      interface{} `json:"data,omitempty"`
}

// Voice represents a voice style
type Voice string

const (
	VoiceF1 Voice = "F1"
	VoiceF2 Voice = "F2"
	VoiceF3 Voice = "F3"
	VoiceF4 Voice = "F4"
	VoiceF5 Voice = "F5"
	VoiceM1 Voice = "M1"
	VoiceM2 Voice = "M2"
	VoiceM3 Voice = "M3"
	VoiceM4 Voice = "M4"
	VoiceM5 Voice = "M5"
)

// Language represents supported languages
type Language string

const (
	LanguageEn Language = "en"
	LanguageEs Language = "es"
	LanguageFr Language = "fr"
	LanguageDe Language = "de"
	LanguageIt Language = "it"
	LanguagePt Language = "pt"
	LanguageJa Language = "ja"
	LanguageZh Language = "zh"
)

// Message represents a conversation message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Config holds orchestrator configuration
type Config struct {
	SampleRate         int
	Channels           int
	BytesPerSamp       int
	MaxContextMessages int
	VoiceStyle         Voice
	Language           Language
	STTTimeout         uint
	LLMTimeout         uint
	TTSTimeout         uint
}

// DefaultConfig returns sensible default configuration
func DefaultConfig() Config {
	return Config{
		SampleRate:         44100,
		Channels:           1,
		BytesPerSamp:       2,
		MaxContextMessages: 20,
		VoiceStyle:         VoiceF1,
		Language:           LanguageEn,
		STTTimeout:         30,
		LLMTimeout:         60,
		TTSTimeout:         30,
	}
}

// ConversationSession represents an ongoing conversation
type ConversationSession struct {
	mu              sync.RWMutex
	ID              string
	Context         []Message
	LastUser        string
	LastAssistant   string
	MaxMessages     int
	CurrentVoice    Voice
	CurrentLanguage Language
}

// NewConversationSession creates a new conversation session
func NewConversationSession(userID string) *ConversationSession {
	return &ConversationSession{
		ID:              userID,
		Context:         []Message{},
		MaxMessages:     20,
		CurrentVoice:    VoiceF1,
		CurrentLanguage: LanguageEn,
	}
}

// AddMessage adds a message to the conversation context
func (s *ConversationSession) AddMessage(role, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = append(s.Context, Message{Role: role, Content: content})
	if len(s.Context) > s.MaxMessages {
		s.Context = s.Context[len(s.Context)-s.MaxMessages:]
	}
	if role == "user" {
		s.LastUser = content
	} else if role == "assistant" {
		s.LastAssistant = content
	}
}

// ClearContext clears the conversation history
func (s *ConversationSession) ClearContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = []Message{}
	s.LastUser = ""
	s.LastAssistant = ""
}

// GetContextCopy returns a thread-safe copy of the conversation context
func (s *ConversationSession) GetContextCopy() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	contextCopy := make([]Message, len(s.Context))
	copy(contextCopy, s.Context)
	return contextCopy
}

// GetCurrentVoice returns the current voice setting thread-safely
func (s *ConversationSession) GetCurrentVoice() Voice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentVoice
}

// GetCurrentLanguage returns the current language setting thread-safely
func (s *ConversationSession) GetCurrentLanguage() Language {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentLanguage
}
