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

type TranscriptionResult struct {
	Text         string
	NoSpeechProb float64 // Probability that the audio contains no speech (0.0 to 1.0)
}

type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error)
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

// RAGProvider is an optional interface for injecting knowledge-base context
// at turn time (LiveKit pattern: retrieve and inject before the LLM call,
// avoiding extra tool round-trips).
type RAGProvider interface {
	Retrieve(ctx context.Context, query string) (string, error)
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
	IsSpeaking() bool
	Reset()
	Clone() VADProvider
	Name() string
}

type VADEventType string

const (
	VADSpeechStart     VADEventType = "SPEECH_START"
	VADSpeechPotential VADEventType = "SPEECH_POTENTIAL"
	VADSpeechEnd       VADEventType = "SPEECH_END"
	VADSilence         VADEventType = "SILENCE"
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
	ToolResult        EventType = "TOOL_RESULT"
	CacheHit          EventType = "CACHE_HIT"
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
	LanguageKo Language = "ko"
	LanguageZh Language = "zh"
	LanguageAr Language = "ar"
	LanguageBg Language = "bg"
	LanguageHr Language = "hr"
	LanguageCs Language = "cs"
	LanguageDa Language = "da"
	LanguageNl Language = "nl"
	LanguageEt Language = "et"
	LanguageFi Language = "fi"
	LanguageEl Language = "el"
	LanguageHi Language = "hi"
	LanguageHu Language = "hu"
	LanguageId Language = "id"
	LanguageLv Language = "lv"
	LanguageLt Language = "lt"
	LanguagePl Language = "pl"
	LanguageRo Language = "ro"
	LanguageRu Language = "ru"
	LanguageSk Language = "sk"
	LanguageSl Language = "sl"
	LanguageSv Language = "sv"
	LanguageTr Language = "tr"
	LanguageUk Language = "uk"
	LanguageVi Language = "vi"
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
	SilenceTimeout           time.Duration

	// PostInterruptBackoff: after a confirmed barge-in, wait this long from
	// the interrupt (not from when the response is ready) before the bot's
	// next reply starts speaking — avoids immediately talking back over a
	// user who paused mid-thought (Vapi backoffSeconds pattern). By the time
	// a reply is ready to speak, STT+LLM processing has usually already
	// eaten a few hundred ms of this window, so it rarely adds its full
	// value on top — but keep it short: it's dead air on every single
	// barge-in, not just edge cases.
	PostInterruptBackoff time.Duration

	// Client-side VAD: server accepts vad_speech_start/end control frames
	ClientVAD bool

	// Token-level TTS: send text to TTS on smaller boundaries (N words or after comma)
	TokenLevelTTS bool

	// Number of words between TTS flushes when TokenLevelTTS is enabled (0 = disabled)
	TTSMinTokenInterval int

	// Speculative LLM: start LLM during speech based on partial audio
	SpeculativeLLM bool

	// Interval (in milliseconds) between speculative STT calls during speech
	SpeculativeIntervalMs int

	// Adaptive pacing: adjust silence timeout based on user speaking rate
	AdaptivePacing bool

	// Response caching: cache common responses to skip LLM entirely
	ResponseCaching bool

	// TTS connection pool size
	TTSConnectionPoolSize int

	// Context summarization: summarize old turns instead of dropping them
	ContextSummarization bool

	// Summarization prompt for context when MaxContextMessages is exceeded
	SummarizationPrompt string

	// STT/LLM/TTS region overrides for co-location
	STTRegion string
	LLMRegion string
	TTSRegion string

	// Vela turn detection: ONNX model path for neural turn detection
	VelaModelPath string

	// Vela thresholds for turn detection decisions
	VelaFloorYieldThreshold   float32 // floor_yield threshold to consider user done (default 0.5)
	VelaContinuationThreshold float32 // continuation threshold below which user is likely done (default 0.4)
	VelaInterruptThreshold    float32 // interruption_safety threshold to allow barge-in (default 0.6)

	// VoiceUXInstructions are appended to the system prompt to instruct the LLM
	// how to format speech for a real-time voice interface. Override for custom behavior.
	VoiceUXInstructions string
}

func DefaultConfig() Config {
	return Config{
		SampleRate:               44100,
		Channels:                 1,
		BytesPerSamp:             2,
		MaxContextMessages:       100,
		VoiceStyle:               VoiceF1,
		MinWordsToInterrupt:      2,
		Language:                 LanguageEn,
		STTTimeout:               30,
		LLMTimeout:               60,
		TTSTimeout:               30,
		BargeInVADThreshold:      0.007,
		BargeInVADTrailWindow:    1500 * time.Millisecond,
		EchoSuppressionThreshold: 0.35,
		FirstSpeaker:             FirstSpeakerBot,
		// Last-resort recovery net: if the session sits idle (or stuck after an
		// interrupt) this long with no user input, monitorInactivity prompts the
		// user again instead of leaving the call silent indefinitely. Previously
		// left at 0 (disabled) except where Telnyx explicitly overrode it.
		SilenceTimeout: 10 * time.Second,
		// Was a hardcoded, unconditional 1s sleep before every post-interrupt
		// reply — halved here since it stacks on top of whatever STT+LLM
		// processing time has already elapsed since the interrupt.
		PostInterruptBackoff: 500 * time.Millisecond,

		ClientVAD:             false,
		TokenLevelTTS:         true,
		TTSMinTokenInterval:   4,
		SpeculativeLLM:        true,
		SpeculativeIntervalMs: 300,
		AdaptivePacing:        true,
		ResponseCaching:       true,
		TTSConnectionPoolSize: 3,
		ContextSummarization:  true,
		SummarizationPrompt:   "Summarize the following conversation turns in 1-2 sentences, keeping key facts and context:",
		STTRegion:             "",
		LLMRegion:             "",
		TTSRegion:             "",

		VelaModelPath:             "assets/onnx/vela/model.onnx",
		VelaFloorYieldThreshold:   0.5,
		VelaContinuationThreshold: 0.4,
		VelaInterruptThreshold:    0.6,
		VoiceUXInstructions:       "",
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
	toolCallCounts  map[string]int // Track how many times each tool has been called
	UserMemory      string         // Cross-call memory extracted from previous sessions
}

func NewConversationSession(userID string) *ConversationSession {
	return &ConversationSession{
		ID:              userID,
		Context:         []Message{},
		MaxMessages:     20,
		CurrentVoice:    VoiceF1,
		CurrentLanguage: LanguageEn,
		toolCallCounts:  make(map[string]int),
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

func (s *ConversationSession) UpdateLastUserMessage(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Context) - 1; i >= 0; i-- {
		if s.Context[i].Role == "user" {
			s.Context[i].Content = content
			s.LastUser = content
			return
		}
	}
	// Fallback if no user message found
	s.Context = append(s.Context, Message{Role: "user", Content: content})
	s.LastUser = content
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

// SummarizeContext removes old messages and replaces them with a summary message
// when the context exceeds the max. Keeps the last keepLast messages intact.
func (s *ConversationSession) SummarizeContext(summaryText string, keepLast int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Context) <= s.MaxMessages {
		return
	}
	trimCount := len(s.Context) - s.MaxMessages
	if keepLast > 0 && trimCount > len(s.Context)-keepLast {
		trimCount = len(s.Context) - keepLast
	}
	if trimCount <= 0 {
		return
	}
	removed := s.Context[:trimCount]
	s.Context = s.Context[trimCount:]

	if summaryText != "" {
		summaryMsg := Message{
			Role:    "system",
			Content: "[Summary of earlier conversation: " + summaryText + "]",
		}
		s.Context = append([]Message{summaryMsg}, s.Context...)
	}
	_ = removed
}

func (s *ConversationSession) NeedsSummarization() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Context) > s.MaxMessages
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
	if s.CurrentLanguage == "na" || s.CurrentLanguage == "auto" {
		return ""
	}
	return s.CurrentLanguage
}

// RecordToolCall increments the call count for a tool and returns true if within limits,
// false if the tool has been called too many times (likely infinite loop).
func (s *ConversationSession) RecordToolCall(toolName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCallCounts[toolName]++
	// Limit tool calls to 3 per tool per session to prevent infinite loops
	return s.toolCallCounts[toolName] <= 3
}

// ResetToolCallCounts clears the tool call history (useful after user input or long pauses).
func (s *ConversationSession) ResetToolCallCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCallCounts = make(map[string]int)
}
