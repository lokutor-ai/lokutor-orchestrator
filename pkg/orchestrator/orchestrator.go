package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
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

func (o *Orchestrator) SetTTSRate(rate float64) {
	type rateSetter interface {
		SetSpeechRate(float64)
	}
	if rs, ok := o.tts.(rateSetter); ok {
		rs.SetSpeechRate(rate)
	}
}

func (o *Orchestrator) GenerateSilent(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	// Try Synthesize (REST) first — avoids WS conflicts with streaming TTS
	audio, err := o.tts.Synthesize(ctx, text, voice, lang)
	if err == nil && len(audio) > 0 {
		return audio, nil
	}

	// Fall back to buffering StreamSynthesize if Synthesize is unavailable
	var buf bytes.Buffer
	if err := o.tts.StreamSynthesize(ctx, text, voice, lang, func(chunk []byte) error {
		buf.Write(chunk)
		return nil
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

CRITICAL: You are speaking in real-time. The user hears your words as you generate them. Follow these rules strictly.

FORMATTING (absolutely required):
- You are SPEAKING, not writing. Never use asterisks, markdown, bullet points, numbered lists, quotes, or any special characters.
- Never write stage directions like "*laughs*", "*pauses*", "*sighs*". Just speak naturally.
- Never use emojis, emoticons, or smileys.
- Say numbers as spoken words: "ten thousand" not "10,000", "twenty dollars" not "$20", "half" not "1/2".
- Never use acronyms or initials. Spell out full names: "American Medical Association" not "AMA", "World Health Organization" not "WHO".
- Use contractions: "don't" not "do not", "can't" not "cannot", "it's" not "it is".
- Start responses directly without preambles like "Absolutely!" or "Of course!" or "Great question!"

SPEAKING STYLE:
- Speak in short, natural sentences like a real person on a phone call. Vary sentence length.
- Pauses should be natural silences in your speech, not words like "um" or "uh" strung together.
- Be warm and conversational, not formal or scripted.
- Match the user's energy level naturally. Never switch languages.
- If you don't know something, just say "I don't know" simply and move on.
- Never explain what you're doing or announce your actions. Just do it.

TOOL USE:
- When you get a tool result, weave it into your response naturally without announcing the lookup.
- Keep tool-related responses brief and direct.
`

func (o *Orchestrator) SetSystemPrompt(session *ConversationSession, prompt string) {
	langInstruction := "IMPORTANT: Always respond in " + string(session.CurrentLanguage) + ". Never switch to another language, even if the user speaks another language. The entire conversation must be in " + string(session.CurrentLanguage) + "."
	fullPrompt := langInstruction + "\n\n" + prompt + "\n\n" + VoiceUXInstructions
	session.AddMessage("system", fullPrompt)
}

func (o *Orchestrator) SetVoice(session *ConversationSession, voice Voice) {
	session.CurrentVoice = voice
}

func (o *Orchestrator) SetLanguage(session *ConversationSession, lang Language) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.CurrentLanguage = lang
	langInstruction := "IMPORTANT: Always respond in " + string(lang) + ". Never switch to another language, even if the user speaks another language. The entire conversation must be in " + string(lang) + "."
	for i, msg := range session.Context {
		if msg.Role == "system" {
			re := regexp.MustCompile(`IMPORTANT: Always respond in[^.]+\.`)
			if re.MatchString(msg.Content) {
				session.Context[i].Content = re.ReplaceAllString(msg.Content, langInstruction)
			} else {
				session.Context[i].Content = msg.Content + "\n\n" + langInstruction
			}
			break
		}
	}
}

func (o *Orchestrator) ResetSession(session *ConversationSession) {
	session.ClearContext()
}

func (o *Orchestrator) NewManagedStream(ctx context.Context, session *ConversationSession) *ManagedStream {
	return NewManagedStream(ctx, o, session)
}
