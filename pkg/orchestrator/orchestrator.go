package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type ToolHandler func(args string) (string, error)

type Orchestrator struct {
	stt    STTProvider
	llm    LLMProvider
	tts    TTSProvider
	vad    VADProvider
	config Config
	logger Logger
	mu     sync.RWMutex

	toolHandlers map[string]ToolHandler
}

// New creates an orchestrator with the given providers and optional logger.
// Logger defaults to NoOpLogger if nil.
func New(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
	if logger == nil {
		logger = &NoOpLogger{}
	}
	return &Orchestrator{
		stt:          stt,
		llm:          llm,
		tts:          tts,
		vad:          vad,
		config:       config,
		logger:       logger,
		toolHandlers: make(map[string]ToolHandler),
	}
}

// NewWithVAD is a convenience constructor that uses a NoOpLogger
func NewWithVAD(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config) *Orchestrator {
	return New(stt, llm, tts, vad, config, nil)
}

func (o *Orchestrator) RegisterTool(name string, handler ToolHandler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolHandlers[name] = handler
}

func (o *Orchestrator) ProcessAudio(ctx context.Context, session *ConversationSession, audioData []byte, streaming bool, onAudioChunk func([]byte) error) (string, []byte, error) {
	transcript, err := o.Transcribe(ctx, audioData, session.GetCurrentLanguage())
	if err != nil {
		return "", nil, fmt.Errorf("transcription failed: %w", err)
	}

	// Reject empty or too-short transcriptions (likely background noise/coughs)
	trimmedText := strings.TrimSpace(transcript.Text)
	if trimmedText == "" {
		o.logger.Warn("empty transcription received", "sessionID", session.ID)
		return "", nil, ErrEmptyTranscription
	}

	// Reject very short text (< 3 chars or single very short word) as likely noise
	// Real speech typically has at least a few words or meaningful length
	if len(trimmedText) < 3 {
		o.logger.Warn("transcription too short - likely noise", "sessionID", session.ID, "text", trimmedText)
		return "", nil, ErrEmptyTranscription
	}

	o.logger.Info("transcription completed", "sessionID", session.ID, "length", len(trimmedText))
	session.AddMessage("user", trimmedText)

	response, err := o.GenerateResponse(ctx, session)
	if err != nil {
		o.logger.Error("LLM generation failed", "sessionID", session.ID, "error", err)
		return transcript.Text, nil, fmt.Errorf("%w: %v", ErrLLMFailed, err)
	}

	o.logger.Info("LLM response generated", "sessionID", session.ID, "length", len(response))
	session.AddMessage("assistant", response)

	audioBytes, err := o.Synthesize(ctx, response, session.GetCurrentVoice(), session.GetCurrentLanguage())
	if err != nil {
		o.logger.Error("TTS synthesis failed", "sessionID", session.ID, "error", err)
		return transcript.Text, nil, fmt.Errorf("%w: %v", ErrTTSFailed, err)
	}

	o.logger.Info("TTS synthesis completed", "sessionID", session.ID, "audioSize", len(audioBytes))

	if streaming && onAudioChunk != nil {
		if err := onAudioChunk(audioBytes); err != nil {
			o.logger.Error("failed to send audio chunk", "error", err)
			return transcript.Text, nil, err
		}
		return transcript.Text, nil, nil
	}
	return transcript.Text, audioBytes, nil
}

// ProcessAudioStream processes audio and streams the TTS response
func (o *Orchestrator) ProcessAudioStream(ctx context.Context, session *ConversationSession, audioData []byte, onAudioChunk func([]byte) error) (string, error) {
	transcript, _, err := o.ProcessAudio(ctx, session, audioData, true, onAudioChunk)
	return transcript, err
}

func (o *Orchestrator) Transcribe(ctx context.Context, audioData []byte, lang Language) (TranscriptionResult, error) {
	return o.stt.Transcribe(ctx, audioData, lang)
}

func (o *Orchestrator) GenerateResponse(ctx context.Context, session *ConversationSession) (string, error) {
	return o.llm.Complete(ctx, session.GetContextCopy(), session.GetTools())
}

func (o *Orchestrator) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return o.tts.Synthesize(ctx, text, voice, lang)
}

func (o *Orchestrator) SynthesizeStream(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	return o.tts.StreamSynthesize(ctx, text, voice, lang, onChunk)
}

func (o *Orchestrator) UpdateConfig(cfg Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.config = cfg
}

func (o *Orchestrator) GetConfig() Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.config
}

func (o *Orchestrator) GetProviders() map[string]string {
	return map[string]string{
		"stt": o.stt.Name(),
		"llm": o.llm.Name(),
		"tts": o.tts.Name(),
	}
}

func (o *Orchestrator) NewSessionWithDefaults(userID string) *ConversationSession {
	session := NewConversationSession(userID)
	session.MaxMessages = o.config.MaxContextMessages
	session.CurrentVoice = o.config.VoiceStyle
	session.CurrentLanguage = o.config.Language
	return session
}

const VoiceUXInstructions = `

IMPORTANT: Real-time voice mode for natural conversation.

Speaking Style:
- Speak naturally and conversationally, like you're talking to someone in person.
- Use natural pauses and breathing patterns in longer sentences.
- Be warm, engaged, and genuinely responsive to what the user says.
- Vary your tone: show curiosity for questions, confidence for statements, warmth for connections.

Fillers & Transitions:
- If you need to think, use brief natural fillers: "Let me think...", "One moment...", "Hmm, interesting..."
- Use natural conversation markers: "So", "Well", "Actually", "You know", "Here's the thing"
- When correcting: "Actually, I should clarify that..." (more human than abrupt changes)

Avoid:
- Long silent pauses (always say something natural)
- Robotic or overly formal language in casual chat
- Repeating the same response structure (keep it varied)

Tool Use:
- Before calling a tool, acknowledge it: "Let me look that up for you..."
- After getting results, naturally integrate them into your response
- Don't announce that you're using a tool - just do it
`

func (o *Orchestrator) SetSystemPrompt(session *ConversationSession, prompt string) {
	fullPrompt := prompt + VoiceUXInstructions
	session.AddMessage("system", fullPrompt)
}

func (o *Orchestrator) SetVoice(session *ConversationSession, voice Voice) {
	session.CurrentVoice = voice
}

func (o *Orchestrator) SetLanguage(session *ConversationSession, lang Language) {
	session.CurrentLanguage = lang
}

func (o *Orchestrator) ResetSession(session *ConversationSession) {
	session.ClearContext()
}

func (o *Orchestrator) NewManagedStream(ctx context.Context, session *ConversationSession) *ManagedStream {
	return NewManagedStream(ctx, o, session)
}
