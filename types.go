package orchestrator

import "context"

// STTProvider is the interface for Speech-to-Text implementations
type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte) (string, error)
	Name() string
}

// LLMProvider is the interface for Language Model implementations
type LLMProvider interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	Name() string
}

// TTSProvider is the interface for Text-to-Speech implementations
type TTSProvider interface {
	Synthesize(ctx context.Context, text string, voice Voice) ([]byte, error)
	StreamSynthesize(ctx context.Context, text string, voice Voice, onChunk func([]byte) error) error
	Name() string
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
		SampleRate:         16000,
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
	s.Context = []Message{}
	s.LastUser = ""
	s.LastAssistant = ""
}
