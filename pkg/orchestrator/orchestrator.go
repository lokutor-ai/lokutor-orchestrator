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



func New(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config) *Orchestrator {
	return NewWithLogger(stt, llm, tts, nil, config, &NoOpLogger{})
}


func NewWithVAD(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config) *Orchestrator {
	return NewWithAllLayers(stt, llm, tts, vad, config, &NoOpLogger{})
}


func NewWithLogger(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
	return NewWithAllLayers(stt, llm, tts, vad, config, logger)
}

func NewWithAllLayers(stt STTProvider, llm LLMProvider, tts TTSProvider, vad VADProvider, config Config, logger Logger) *Orchestrator {
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

func (o *Orchestrator) RegisterTool(name string, handler ToolHandler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolHandlers[name] = handler
}


func (o *Orchestrator) PushAudio(sessionID string, chunk []byte) (*VADEvent, error) {
	if o.vad == nil {
		return nil, fmt.Errorf("VAD provider not configured")
	}
	return o.vad.Process(chunk)
}


func (o *Orchestrator) ProcessAudio(ctx context.Context, session *ConversationSession, audioData []byte) (string, []byte, error) {
	
	transcript, err := o.Transcribe(ctx, audioData, session.GetCurrentLanguage())
	if err != nil {
		return "", nil, fmt.Errorf("transcription failed: %w", err)
	}

	if strings.TrimSpace(transcript) == "" {
		o.logger.Warn("empty transcription received", "sessionID", session.ID)
		return "", nil, ErrEmptyTranscription
	}

	o.logger.Info("transcription completed", "sessionID", session.ID, "length", len(transcript))
	session.AddMessage("user", transcript)

	
	response, err := o.GenerateResponse(ctx, session)
	if err != nil {
		o.logger.Error("LLM generation failed", "sessionID", session.ID, "error", err)
		return transcript, nil, fmt.Errorf("%w: %v", ErrLLMFailed, err)
	}

	o.logger.Info("LLM response generated", "sessionID", session.ID, "length", len(response))
	session.AddMessage("assistant", response)

	
	audioBytes, err := o.Synthesize(ctx, response, session.GetCurrentVoice(), session.GetCurrentLanguage())
	if err != nil {
		o.logger.Error("TTS synthesis failed", "sessionID", session.ID, "error", err)
		return transcript, nil, fmt.Errorf("%w: %v", ErrTTSFailed, err)
	}

	o.logger.Info("TTS synthesis completed", "sessionID", session.ID, "audioSize", len(audioBytes))
	return transcript, audioBytes, nil
}


func (o *Orchestrator) ProcessAudioStream(ctx context.Context, session *ConversationSession, audioData []byte, onAudioChunk func([]byte) error) (string, error) {
	
	transcript, err := o.Transcribe(ctx, audioData, session.GetCurrentLanguage())
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}

	if strings.TrimSpace(transcript) == "" {
		o.logger.Warn("empty transcription received", "sessionID", session.ID)
		return "", ErrEmptyTranscription
	}

	o.logger.Info("transcription completed", "sessionID", session.ID, "length", len(transcript))
	session.AddMessage("user", transcript)

	
	response, err := o.GenerateResponse(ctx, session)
	if err != nil {
		o.logger.Error("LLM generation failed", "sessionID", session.ID, "error", err)
		return transcript, fmt.Errorf("%w: %v", ErrLLMFailed, err)
	}

	o.logger.Info("LLM response generated", "sessionID", session.ID, "length", len(response))
	session.AddMessage("assistant", response)

	
	err = o.SynthesizeStream(ctx, response, session.GetCurrentVoice(), session.GetCurrentLanguage(), onAudioChunk)
	if err != nil {
		o.logger.Error("TTS streaming failed", "sessionID", session.ID, "error", err)
		return transcript, fmt.Errorf("%w: %v", ErrTTSFailed, err)
	}

	o.logger.Info("TTS streaming completed", "sessionID", session.ID)
	return transcript, nil
}


func (o *Orchestrator) Transcribe(ctx context.Context, audioData []byte, lang Language) (string, error) {
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


func (o *Orchestrator) HandleInterruption(session *ConversationSession) {
	o.logger.Info("conversation interrupted", "sessionID", session.ID)
	
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



func (o *Orchestrator) SetSystemPrompt(session *ConversationSession, prompt string) {
	session.AddMessage("system", prompt)
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
