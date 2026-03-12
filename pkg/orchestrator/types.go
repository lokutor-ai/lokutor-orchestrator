package orchestrator

import (
	"context"
	"sync"
	"time"
)

type Logger interface {
	Debug(msg string, args ...interface{})

	Info(msg string, args ...interface{})

	Warn(msg string, args ...interface{})

	Error(msg string, args ...interface{})
}

type NoOpLogger struct{}

func (n *NoOpLogger) Debug(msg string, args ...interface{}) {}
func (n *NoOpLogger) Info(msg string, args ...interface{})  {}
func (n *NoOpLogger) Warn(msg string, args ...interface{})  {}
func (n *NoOpLogger) Error(msg string, args ...interface{}) {}

type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte, lang Language) (string, error)
	Name() string
}

type StreamingSTTProvider interface {
	STTProvider
	StreamTranscribe(ctx context.Context, lang Language, onTranscript func(transcript string, isFinal bool) error) (chan<- []byte, error)
}

type LLMProvider interface {
	Complete(ctx context.Context, messages []Message, tools []Tool) (string, error)
	Name() string
}

type StreamingLLMProvider interface {
	LLMProvider
	StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error)
}


type TTSProvider interface {
	Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error)
	StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error
	Abort() error
	Name() string
}

type VADProvider interface {
	Process(chunk []byte) (*VADEvent, error)
	Reset()
	Clone() VADProvider
	Name() string
}

type VADEventType string

const (
	VADSpeechStart VADEventType = "SPEECH_START"
	VADSpeechEnd   VADEventType = "SPEECH_END"
	VADSilence     VADEventType = "SILENCE"
)

type VADEvent struct {
	Type      VADEventType
	Timestamp int64
}

type EventType string

const (
	UserSpeaking      EventType = "USER_SPEAKING"
	UserStopped       EventType = "USER_STOPPED"
	TranscriptPartial EventType = "TRANSCRIPT_PARTIAL"
	TranscriptFinal   EventType = "TRANSCRIPT_FINAL"
	BotThinking       EventType = "BOT_THINKING"
	BotResponse       EventType = "BOT_RESPONSE"
	BotSpeaking       EventType = "BOT_SPEAKING"
	Interrupted       EventType = "INTERRUPTED"
	BotResumed        EventType = "BOT_RESUMED"
	AudioChunk        EventType = "AUDIO_CHUNK"
	ToolCall          EventType = "TOOL_CALL"
	ErrorEvent        EventType = "ERROR"
)

type ToolCallEventData struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
}

type OrchestratorEvent struct {
	Type       EventType   `json:"type"`
	SessionID  string      `json:"session_id"`
	Data       interface{} `json:"data,omitempty"`
	Generation int         `json:"generation,omitempty"`
}

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

type Message struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	Name       string      `json:"name,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolCalls  interface{} `json:"tool_calls,omitempty"`
}

type Tool struct {
	Type     string      `json:"type"` // e.g. "function"
	Function interface{} `json:"function"`
}

type FirstSpeaker string

const (
	FirstSpeakerUser FirstSpeaker = "user"
	FirstSpeakerBot  FirstSpeaker = "bot"
)

type Config struct {
	SampleRate               int
	Channels                 int
	BytesPerSamp             int
	MaxContextMessages       int
	VoiceStyle               Voice
	MinWordsToInterrupt      int
	Language                 Language
	STTTimeout               uint
	LLMTimeout               uint
	TTSTimeout               uint
	BargeInVADThreshold      float64
	BargeInVADTrailWindow    time.Duration
	EchoSuppressionThreshold float64
	FirstSpeaker             FirstSpeaker
}

func DefaultConfig() Config {
	return Config{
		SampleRate:               44100,
		Channels:                 1,
		BytesPerSamp:             2,
		MaxContextMessages:       20,
		VoiceStyle:               VoiceF1,
		MinWordsToInterrupt:      1,
		Language:                 LanguageEn,
		STTTimeout:               30,
		LLMTimeout:               60,
		TTSTimeout:               30,
		BargeInVADThreshold:      0.007,
		BargeInVADTrailWindow:    1500 * time.Millisecond,
		EchoSuppressionThreshold: 0.35,
		FirstSpeaker:             FirstSpeakerBot,
	}
}

type ConversationSession struct {
	mu              sync.RWMutex
	ID              string
	Context         []Message
	LastUser        string
	LastAssistant   string
	MaxMessages     int
	CurrentVoice    Voice
	CurrentLanguage Language
	Tools           []Tool
}

func NewConversationSession(userID string) *ConversationSession {
	return &ConversationSession{
		ID:              userID,
		Context:         []Message{},
		MaxMessages:     20,
		CurrentVoice:    VoiceF1,
		CurrentLanguage: LanguageEn,
	}
}

func (s *ConversationSession) AddMessage(role, content string) {
	s.AddMessageRaw(Message{Role: role, Content: content})
}

func (s *ConversationSession) AddMessageRaw(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = append(s.Context, msg)
	if len(s.Context) > s.MaxMessages {
		s.Context = s.Context[len(s.Context)-s.MaxMessages:]
	}
	if msg.Role == "user" {
		s.LastUser = msg.Content
	} else if msg.Role == "assistant" && msg.Content != "" {
		s.LastAssistant = msg.Content
	}
}

func (s *ConversationSession) SetTools(tools []Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tools = tools
}

func (s *ConversationSession) GetTools() []Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Tools
}

func (s *ConversationSession) ClearContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = []Message{}
	s.LastUser = ""
	s.LastAssistant = ""
}

func (s *ConversationSession) GetContextCopy() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	contextCopy := make([]Message, len(s.Context))
	copy(contextCopy, s.Context)
	return contextCopy
}

func (s *ConversationSession) GetCurrentVoice() Voice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentVoice
}

func (s *ConversationSession) GetCurrentLanguage() Language {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentLanguage
}
