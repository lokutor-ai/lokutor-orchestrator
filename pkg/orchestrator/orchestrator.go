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

// New creates an orchestrator with STT, LLM, TTS providers and config.
// VAD and Logger default to nil/NoOpLogger.
func New(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config) *Orchestrator {
	return newOrchestrator(stt, llm, tts, nil, config, nil)
}

// NewWithVAD creates an orchestrator with all providers including VAD.
func NewWithVAD(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config) *Orchestrator {
	return newOrchestrator(stt, llm, tts, vad, config, nil)
}

// NewWithLogger creates an orchestrator with all providers, VAD, and logger.
func NewWithLogger(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
	return newOrchestrator(stt, llm, tts, vad, config, logger)
}

func NewWithAllLayers(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
	return newOrchestrator(stt, llm, tts, vad, config, logger)
}

func newOrchestrator(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
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

func (o *Orchestrator) GetLLMProvider() LLMProvider {
	return o.llm
}

func (o *Orchestrator) SummarizeContext(ctx context.Context, session *ConversationSession) error {
	if o.llm == nil {
		return fmt.Errorf("no LLM provider")
	}
	messages := session.GetContextCopy()

	var turnsToSummarize []Message
	for _, msg := range messages {
		if msg.Role == "system" && strings.HasPrefix(msg.Content, "[Summary") {
			continue
		}
		if msg.Role == "user" || msg.Role == "assistant" {
			turnsToSummarize = append(turnsToSummarize, msg)
		}
	}

	if len(turnsToSummarize) < 2 {
		return nil
	}

	var sb strings.Builder
	for _, msg := range turnsToSummarize {
		content := msg.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		sb.WriteString(msg.Role + ": " + content + "\n")
	}

	prompt := o.config.SummarizationPrompt + "\n\n" + sb.String()
	summaryMessages := []Message{
		{Role: "system", Content: "You generate concise summaries of conversations. Keep key facts and context, max 3 sentences."},
		{Role: "user", Content: prompt},
	}

	summary, err := o.llm.Complete(ctx, summaryMessages, nil)
	if err != nil || summary == "" {
		return err
	}

	session.SummarizeContext(summary, session.MaxMessages/2)
	return nil
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

// transcribeNoFilter is implemented by STTWrapper to bypass noise suppression.
type transcribeNoFilter interface {
	TranscribeNoFilter(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error)
}

// TranscribeRaw bypasses noise suppression for fast, unfiltered transcription.
// Used by speculative STT where speed matters more than full noise suppression.
func (o *Orchestrator) TranscribeRaw(ctx context.Context, audioData []byte, lang Language) (TranscriptionResult, error) {
	if nf, ok := o.stt.(transcribeNoFilter); ok {
		return nf.TranscribeNoFilter(ctx, audioData, lang)
	}
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
CRITICAL: You are speaking out loud in real-time. Follow these rules exactly.

SPOKEN FORMAT RULES:
- Use contractions always: don't, can't, I'll, it's, won't, they're, we're, isn't
- Use casual spoken words: yeah, okay, a lot, kind of, a bit
- Never use written punctuation like asterisks, bullet points, numbered lists, markdown, quotes, emojis
- Never write stage directions like *laughs* or *sighs*
- Never use acronyms or abbreviations — spell out full names
- Write numbers as spoken words: say "about a hundred" not 100, "half" not 1/2

SPEAKING STYLE RULES:
- Start your response immediately with the answer. No preambles like "Absolutely!", "Great question!", "Of course!", "That's a good point."
- Keep sentences short and conversational. Vary long and short sentences.
- Never announce what you are going to do. Do not say "Let me check" or "I'll look into that." Just give the result.
- Never explain your process. The user wants the answer, not the steps.
- Never use formal transition words like "firstly", "secondly", "in conclusion", "furthermore"
- Never apologize unless you actually made a mistake
- If you don't know something, say "I don't know" simply.
- Sound warm but not fake. Match how casual or serious the user sounds.
- Answer first, then add details if needed. Do not start with background context.
- Use natural uncertainty when appropriate: "I think", "I'm pretty sure", "not entirely sure but"
- Vary your sentence openings. Do not start every response the same way.

TOOL USE:
- When you get a tool result, give the answer directly without mentioning the tool or lookup
`

func (o *Orchestrator) SetSystemPrompt(session *ConversationSession, prompt string) {
	// Map language code to human-readable name for LLM instruction
	langName := languageCodeToName(session.CurrentLanguage)
	langInstruction := "IMPORTANT: Always respond in " + langName + ". Never switch to another language, even if the user speaks another language. The entire conversation must be in " + langName + "."
	uxInstructions := o.config.VoiceUXInstructions
	if uxInstructions == "" {
		uxInstructions = VoiceUXInstructions
	}
	fullPrompt := langInstruction + "\n\n" + prompt + "\n\n" + uxInstructions
	session.AddMessage("system", fullPrompt)
}

func (o *Orchestrator) SetVoice(session *ConversationSession, voice Voice) {
	session.CurrentVoice = voice
}

func (o *Orchestrator) SetLanguage(session *ConversationSession, lang Language) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.CurrentLanguage = lang
	
	// Map language code to human-readable name for LLM instruction
	langName := languageCodeToName(lang)
	langInstruction := "IMPORTANT: Always respond in " + langName + ". Never switch to another language, even if the user speaks another language. The entire conversation must be in " + langName + "."
	
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

// languageCodeToName maps language codes to human-readable names for LLM prompts.
func languageCodeToName(lang Language) string {
	switch lang {
	case LanguageEn:
		return "English"
	case LanguageEs:
		return "Spanish"
	case LanguageFr:
		return "French"
	case LanguageDe:
		return "German"
	case LanguageIt:
		return "Italian"
	case LanguagePt:
		return "Portuguese"
	case LanguageJa:
		return "Japanese"
	case LanguageKo:
		return "Korean"
	case LanguageZh:
		return "Chinese"
	case LanguageAr:
		return "Arabic"
	case LanguageBg:
		return "Bulgarian"
	case LanguageHr:
		return "Croatian"
	case LanguageCs:
		return "Czech"
	case LanguageDa:
		return "Danish"
	case LanguageNl:
		return "Dutch"
	case LanguageEt:
		return "Estonian"
	case LanguageFi:
		return "Finnish"
	case LanguageEl:
		return "Greek"
	case LanguageHi:
		return "Hindi"
	case LanguageHu:
		return "Hungarian"
	case LanguageId:
		return "Indonesian"
	case LanguageLv:
		return "Latvian"
	case LanguageLt:
		return "Lithuanian"
	case LanguagePl:
		return "Polish"
	case LanguageRo:
		return "Romanian"
	case LanguageRu:
		return "Russian"
	case LanguageSk:
		return "Slovak"
	case LanguageSl:
		return "Slovenian"
	case LanguageSv:
		return "Swedish"
	case LanguageTr:
		return "Turkish"
	case LanguageUk:
		return "Ukrainian"
	case LanguageVi:
		return "Vietnamese"
	default:
		return string(lang)
	}
}

func (o *Orchestrator) ResetSession(session *ConversationSession) {
	session.ClearContext()
}

func (o *Orchestrator) NewManagedStream(ctx context.Context, session *ConversationSession) *ManagedStream {
	return NewManagedStream(ctx, o, session)
}
