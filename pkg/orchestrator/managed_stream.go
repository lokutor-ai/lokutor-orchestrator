package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/prosody"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/vela"
)

type StreamState int

const (
	StateIdle       StreamState = iota
	StateListening
	StateProcessing
	StateSpeaking
	StateInterrupted
)

type ManagedStream struct {
	orch    *Orchestrator
	session *ConversationSession
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan OrchestratorEvent
	vad     VADProvider

	// Vela turn detection model (replaces VAD-based turn detection)
	vela *vela.Detector

	cmdChan          chan []byte
	interruptChan    chan struct{}
	state            StreamState
	stateMu          sync.Mutex

	audioBuf  *bytes.Buffer
	userAudio []byte

	userSpeakingSince time.Time
	userSpeechEnd     time.Time
	lastUserText      string

	vadSpeaking bool

	pipelineCancel context.CancelFunc
	ttsCancel      context.CancelFunc

	payloadGen int

	logger Logger

	prosody     *prosody.AdaptiveProcessor
	userProfile *prosody.UserSpeechProfile
	turnComp    *TurnCompletionAnalyzer
	backch      *BackchannelDetector

	playbackRate    int
	inputSampleRate int
	isClosed        bool
	closeOnce       sync.Once

	sttStartTime       time.Time
	sttEndTime         time.Time
	llmStartTime       time.Time
	llmEndTime         time.Time
	ttsStartTime       time.Time
	ttsFirstChunkTime  time.Time
	ttsEndTime         time.Time
	botSpeakStart      time.Time
	lastAudioSentAt    time.Time
	lastNoSpeechProb   float64
	lastActivityAt     time.Time

	// Client-side VAD support
	controlChan  chan []byte
	clientVAD    bool

	// Speculative LLM execution during speech
	speculator     *SpeculativeExecutor
	lastSpecAt     time.Time
	speechAudioBuf []byte

	// preSpeechBuf stores the last ~300ms of audio unconditionally, updated BEFORE VAD.
	// Used in onVADStart to prepend speech onset that VAD's confirmation window missed.
	preSpeechBuf *bytes.Buffer

	// Response cache
	responseCache *ResponseCache

	// Adaptive pacing
	speakingRateWindow []float64

	// Utterance sequence counter: incremented on each VADSpeechEnd.
	// Used to skip LLM for older utterances when consecutive speech arrives,
	// so the newest utterance's LLM call sees all accumulated context.
	utteranceSeq int

	// Bot speech deduplication: tracks the generation of the last BotSpeaking emission
	// to prevent emitting the same event multiple times for a single generation.
	lastBotSpeakGen int

	// ttsMu serializes TTS operations within this session to prevent concurrent
	// WS frame corruption when multiple sentences are queued for synthesis.
	// Deepgram WS is not thread-safe, so we ensure only one StreamSynthesize
	// call at a time per session.
	ttsMu sync.Mutex

	mu sync.Mutex
}

func NewManagedStream(ctx context.Context, o *Orchestrator, session *ConversationSession) *ManagedStream {
	mCtx, mCancel := context.WithCancel(ctx)

	cfg := DefaultConfig()
	if o != nil {
		cfg = o.GetConfig()
	}

	var streamVAD VADProvider
	if o != nil && o.vad != nil {
		streamVAD = o.vad.Clone()
	}

	logger := o.logger
	if logger == nil {
		logger = &NoOpLogger{}
	}

	ms := &ManagedStream{
		orch:            o,
		session:         session,
		ctx:             mCtx,
		cancel:          mCancel,
		events:          make(chan OrchestratorEvent, 1024),
		cmdChan:         make(chan []byte, 512),
		interruptChan:   make(chan struct{}, 1),
		audioBuf:        new(bytes.Buffer),
		vad:             streamVAD,
		playbackRate:    44100,
		inputSampleRate: cfg.SampleRate,
		turnComp: NewTurnCompletionAnalyzer(),
		userProfile:     prosody.NewUserSpeechProfile(),
		prosody: func() *prosody.AdaptiveProcessor {
			c := prosody.DefaultConfig()
			c.ThinkerMode = true
			c.EmphasisLevel = 0.6
			return prosody.NewAdaptiveProcessor(c)
		}(),
		logger:        logger,
		lastActivityAt: time.Now(),
		controlChan:   make(chan []byte, 64),
		clientVAD:     cfg.ClientVAD,
		speculator:     NewSpeculativeExecutor(cfg.SpeculativeIntervalMs),
		speechAudioBuf: make([]byte, 0, 44100),
		speakingRateWindow: make([]float64, 0, 20),
		preSpeechBuf:   bytes.NewBuffer(make([]byte, 0, 300*44100*2/1000)),
	}

	// Initialize Vela turn detection model if path is configured
	if cfg.VelaModelPath != "" {
		if _, err := os.Stat(cfg.VelaModelPath); err == nil {
			v, err := vela.NewDetector(cfg.VelaModelPath)
			if err != nil {
				logger.Warn("failed to load Vela model, falling back to VAD", "error", err)
			} else {
				ms.vela = v
				logger.Info("Vela turn detection loaded", "model", cfg.VelaModelPath)
			}
		} else {
			logger.Warn("Vela model file not found, falling back to VAD", "path", cfg.VelaModelPath)
		}
	}

	if cfg.ResponseCaching {
		ms.responseCache = NewResponseCache(5*time.Minute, 100)
	}

	if cfg.SpeculativeLLM && ms.speculator != nil {
		ms.speculator.SetOnPartial(func(partial string) {
			ms.emit(TranscriptPartial, partial)
		})
	}

	detector := NewBackchannelDetector(DefaultBackchannelConfig(), 44100, func(raw []byte) {
		ms.emitBackchannel(raw)
	})
	detector.clips = make([][]byte, 0)
	ms.backch = detector

	go ms.audioProcessor()
	go ms.monitorInactivity()

	if o != nil && o.tts != nil {
		go ms.generateBackchannelClips(o)
	}

	if o != nil && o.config.FirstSpeaker == FirstSpeakerBot {
		go func() {
			time.Sleep(600 * time.Millisecond)
			greeting := "Hello!"
			if o.config.Language == LanguageEs {
				greeting = "¡Hola!"
			}
			ms.session.AddMessage("assistant", greeting)
			ms.runLLMAndTTS(ms.ctx, greeting)
		}()
	}

	return ms
}

func (ms *ManagedStream) audioProcessor() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[PANIC] audioProcessor: %v\n", r)
		}
	}()

	for {
		select {
		case <-ms.ctx.Done():
			return
		case <-ms.interruptChan:
			ms.handleInterrupt()
		case chunk := <-ms.cmdChan:
			ms.handleAudio(chunk)
		case ctrl := <-ms.controlChan:
			ms.handleControl(ctrl)
		}
	}
}

func (ms *ManagedStream) handleAudio(chunk []byte) {
	ms.mu.Lock()
	state := ms.state
	clientVAD := ms.clientVAD
	// Update pre-speech buffer BEFORE VAD processing so it never includes
	// the current chunk when onVADStart reads it later in this call.
	ms.preSpeechBuf.Write(chunk)
	maxPreSpeech := 300 * int(ms.inputSampleRate) * 2 / 1000
	if ms.preSpeechBuf.Len() > maxPreSpeech {
		data := ms.preSpeechBuf.Bytes()
		keep := data[len(data)-maxPreSpeech:]
		ms.preSpeechBuf.Reset()
		ms.preSpeechBuf.Write(keep)
	}
	ms.mu.Unlock()

	// In client VAD mode, the client sends control frames for speech boundaries.
	// The audio processor only buffers audio and runs backchannel detection.
	if clientVAD {
		ms.mu.Lock()
		ms.audioBuf.Write(chunk)
		if ms.audioBuf.Len() > 176400 {
			data := ms.audioBuf.Bytes()
			leadIn := data[len(data)-132300:]
			ms.audioBuf.Reset()
			ms.audioBuf.Write(leadIn)
		}
		ms.mu.Unlock()

		isSpeaking := ms.vadSpeaking
		if isSpeaking {
			ms.userAudio = append(ms.userAudio, chunk...)
			ms.speechAudioBuf = append(ms.speechAudioBuf, chunk...)

			if ms.speculator != nil && ms.orch.config.SpeculativeLLM {
				speechDuration := time.Since(ms.userSpeakingSince)
				if ms.speculator.ShouldSpeculate(speechDuration, ms.lastSpecAt) {
					ms.lastSpecAt = time.Now()
					audioCopy := make([]byte, len(ms.speechAudioBuf))
					copy(audioCopy, ms.speechAudioBuf)
					ms.speculator.Start(ms.ctx, ms.orch, audioCopy, ms.session.GetCurrentLanguage())
				}
			}
		}

		if ms.backch != nil && isSpeaking && len(chunk) >= 80 {
			samples := make([]int16, len(chunk)/2)
			for i := range samples {
				samples[i] = int16(chunk[i*2]) | int16(chunk[i*2+1])<<8
			}
			ms.backch.ProcessAudio(samples, time.Now())
		}

		if isSpeaking {
			ms.updateActivity()
		}
		return
	}

	// Vela turn detection mode: use neural model instead of VAD
	if ms.vela != nil {
		ms.handleAudioVela(chunk, state)
		return
	}

	// Legacy VAD mode
	if ms.vad == nil {
		return
	}

	event, err := ms.vad.Process(chunk)
	if err != nil {
		return
	}

	isSpeaking := ms.vad.IsSpeaking()
	ms.vadSpeaking = isSpeaking

	if event != nil && (event.Type == VADSpeechStart || event.Type == VADSpeechEnd || event.Type == VADSpeechPotential) {
		ms.logger.Info("VAD event",
			"type", event.Type,
			"state", state)
	}

	if isSpeaking {
		ms.userAudio = append(ms.userAudio, chunk...)
		ms.speechAudioBuf = append(ms.speechAudioBuf, chunk...)

		if ms.speculator != nil && ms.orch.config.SpeculativeLLM {
			speechDuration := time.Since(ms.userSpeakingSince)
			if ms.speculator.ShouldSpeculate(speechDuration, ms.lastSpecAt) {
				ms.lastSpecAt = time.Now()
				audioCopy := make([]byte, len(ms.speechAudioBuf))
				copy(audioCopy, ms.speechAudioBuf)
				ms.speculator.Start(ms.ctx, ms.orch, audioCopy, ms.session.GetCurrentLanguage())
			}
		}
	}

	ms.mu.Lock()
	ms.audioBuf.Write(chunk)
	if ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-132300:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}
	ms.mu.Unlock()

	switch {
	case event != nil && event.Type == VADSpeechStart:
		ms.onVADStart(state)
	case event != nil && event.Type == VADSpeechEnd:
		ms.onVADEnd(state)
	}

	if ms.backch != nil && isSpeaking && len(chunk) >= 80 {
		samples := make([]int16, len(chunk)/2)
		for i := range samples {
			samples[i] = int16(chunk[i*2]) | int16(chunk[i*2+1])<<8
		}
		ms.backch.ProcessAudio(samples, time.Now())
	}

	if isSpeaking {
		ms.updateActivity()
	}
}

// handleAudioVela processes audio using the Vela neural turn detection model.
func (ms *ManagedStream) handleAudioVela(chunk []byte, prevState StreamState) {
	// Convert input sample rate to 16kHz for Vela if needed
	audioChunk := chunk
	if ms.inputSampleRate != 16000 {
		// Resample to 16kHz (simple linear interpolation)
		audioChunk = resampleTo16k(chunk, ms.inputSampleRate)
	}

	// Vela expects int16 PCM at 16kHz, 320 samples per frame (20ms)
	// The chunk from the client is at ms.inputSampleRate (typically 44100Hz)
	// We need to convert and buffer to get exactly 320 samples at 16kHz

	// For now, process the chunk directly - Vela handles the conversion internally
	event, err := ms.vela.Process(audioChunk)
	if err != nil {
		ms.logger.Warn("Vela processing error", "error", err)
		return
	}

	isSpeaking := ms.vela.IsSpeaking()
	ms.vadSpeaking = isSpeaking

	cfg := ms.orch.GetConfig()

	// Vela turn detection logic:
	// - floor_yield > threshold AND continuation < threshold → user is done
	// - floor_yield < threshold AND continuation > threshold → user is speaking
	// - interruption_safety > threshold → safe to interrupt

	if isSpeaking {
		ms.userAudio = append(ms.userAudio, chunk...)
		ms.speechAudioBuf = append(ms.speechAudioBuf, chunk...)

		if ms.speculator != nil && ms.orch.config.SpeculativeLLM {
			speechDuration := time.Since(ms.userSpeakingSince)
			if ms.speculator.ShouldSpeculate(speechDuration, ms.lastSpecAt) {
				ms.lastSpecAt = time.Now()
				audioCopy := make([]byte, len(ms.speechAudioBuf))
				copy(audioCopy, ms.speechAudioBuf)
				ms.speculator.Start(ms.ctx, ms.orch, audioCopy, ms.session.GetCurrentLanguage())
			}
		}
	}

	ms.mu.Lock()
	ms.audioBuf.Write(chunk)
	if ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-132300:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}
	ms.mu.Unlock()

	// Check for turn completion (user is done speaking)
	if event.FloorYield > cfg.VelaFloorYieldThreshold &&
		event.Continuation < cfg.VelaContinuationThreshold &&
		isSpeaking == false {
		// User has yielded the floor and is not speaking → trigger speech end
		ms.onVADEnd(prevState)
		return
	}

	// Check for speech start (user started speaking)
	if event.Continuation > cfg.VelaContinuationThreshold && isSpeaking {
		// User is speaking → trigger speech start if not already
		if prevState != StateListening && prevState != StateProcessing {
			ms.onVADStart(prevState)
		}
	}

	// Check for safe interruption (user wants to interrupt bot)
	if event.InterruptionSafety > cfg.VelaInterruptThreshold && isSpeaking {
		if prevState == StateSpeaking || prevState == StateProcessing {
			ms.emit(UserSpeaking, nil)
			ms.cancelPipeline()
			return
		}
	}

	if ms.backch != nil && isSpeaking && len(chunk) >= 80 {
		samples := make([]int16, len(chunk)/2)
		for i := range samples {
			samples[i] = int16(chunk[i*2]) | int16(chunk[i*2+1])<<8
		}
		ms.backch.ProcessAudio(samples, time.Now())
	}

	if isSpeaking {
		ms.updateActivity()
	}
}

// resampleTo16k resamples audio from the input sample rate to 16kHz using linear interpolation.
func resampleTo16k(audio []byte, inputSampleRate int) []byte {
	if inputSampleRate == 16000 {
		return audio
	}

	// Calculate output length
	ratio := float64(16000) / float64(inputSampleRate)
	outLen := int(float64(len(audio)/2) * ratio * 2)
	if outLen%2 != 0 {
		outLen--
	}
	if outLen <= 0 {
		return audio
	}

	out := make([]byte, outLen)
	inSamples := len(audio) / 2

	for i := 0; i < outLen/2; i++ {
		srcPos := float64(i) / ratio
		srcIdx := int(srcPos)
		frac := srcPos - float64(srcIdx)

		if srcIdx >= inSamples-1 {
			srcIdx = inSamples - 2
		}

		// Linear interpolation
		s0 := int16(audio[srcIdx*2]) | int16(audio[srcIdx*2+1])<<8
		s1 := int16(audio[(srcIdx+1)*2]) | int16(audio[(srcIdx+1)*2+1])<<8
		sample := int16(float64(s0)*(1-frac) + float64(s1)*frac)

		out[i*2] = byte(sample)
		out[i*2+1] = byte(sample >> 8)
	}

	return out
}

func (ms *ManagedStream) onVADStart(prevState StreamState) {
	// Cooldown: ignore VAD start if a speech end happened <500ms ago.
	// Prevents VAD from immediately re-triggering on residual audio / echo
	// after a user utterance ends (causing duplicate STT calls).
	if !ms.userSpeechEnd.IsZero() && time.Since(ms.userSpeechEnd) < 500*time.Millisecond {
		ms.logger.Info("VAD start ignored (cooldown)", "since_end_ms", time.Since(ms.userSpeechEnd).Milliseconds())
		ms.vad.Reset()
		return
	}

	ms.userSpeakingSince = time.Now()

	// Prepend 300ms of pre-speech audio to capture the speech onset
	// that VAD may have missed during its confirmation window (first ~1-2 chunks).
	// preSpeechBuf is updated BEFORE VAD in handleAudio, so it never includes
	// the current chunk — no duplicates in userAudio.
	ms.mu.Lock()
	if ms.preSpeechBuf.Len() > 0 {
		buf := ms.preSpeechBuf.Bytes()
		leadIn := make([]byte, len(buf))
		copy(leadIn, buf)
		ms.userAudio = append(leadIn, ms.userAudio...)
	}
	ms.mu.Unlock()

	if ms.backch != nil {
		ms.backch.UserStarted()
	}

	ms.mu.Lock()
	ms.state = StateListening
	ms.mu.Unlock()

	if prevState == StateSpeaking || prevState == StateProcessing {
		ms.emit(UserSpeaking, nil)
		ms.cancelPipeline()
		return
	}

	ms.emit(UserSpeaking, nil)
}

func (ms *ManagedStream) onVADEnd(prevState StreamState) {
	ms.userSpeechEnd = time.Now()
	ms.emit(UserStopped, nil)

	if ms.backch != nil {
		ms.backch.UserStarted()
		ms.backch.UserStopped()
	}

	duration := ms.userSpeechEnd.Sub(ms.userSpeakingSince)
	audioData := ms.userAudio
	ms.userAudio = nil

	speechAudio := ms.speechAudioBuf
	ms.speechAudioBuf = nil

	// Adaptive VAD: if energy was rising before speech end, the user is likely
	// pausing mid-thought — extend the minimum duration to avoid splitting
	// consecutive sentences across separate turns.
	minDur := 200 * time.Millisecond
	minLen := 160
	if !ms.clientVAD {
		if trendVAD, ok := ms.vad.(interface{ GetEnergyTrend() float64 }); ok {
			trend := trendVAD.GetEnergyTrend()
			if trend > 0.0005 {
				minDur = 450 * time.Millisecond
				minLen = 320
				ms.logger.Debug("Adaptive VAD: energy rising, extending silence window",
					"trend", trend, "minDur", minDur.String())
			}
		}
	}

	// Adaptive pacing: adjust silence limit based on speaking rate
	if ms.orch.config.AdaptivePacing && len(speechAudio) > 0 {
		words := countWords(string(audioData))
		if words > 1 {
			ms.speakingRateWindow = append(ms.speakingRateWindow, float64(words)/duration.Seconds())
			if len(ms.speakingRateWindow) > 10 {
				ms.speakingRateWindow = ms.speakingRateWindow[1:]
			}
			var avgRate float64
			for _, r := range ms.speakingRateWindow {
				avgRate += r
			}
			if len(ms.speakingRateWindow) > 0 {
				avgRate /= float64(len(ms.speakingRateWindow))
			}
			if avgRate > 3.5 {
				minDur = 150 * time.Millisecond
				minLen = 120
				ms.logger.Debug("Adaptive pacing: fast talker, shorter silence window",
					"rate", avgRate, "minDur", minDur.String())
			} else if avgRate < 1.5 {
				minDur = 350 * time.Millisecond
				minLen = 240
				ms.logger.Debug("Adaptive pacing: slow talker, longer silence window",
					"rate", avgRate, "minDur", minDur.String())
			}
		}
	}

	if duration < minDur || len(audioData) < minLen {
		return
	}

	// Cancel any in-flight speculation before final processing
	if ms.speculator != nil && ms.orch.config.SpeculativeLLM {
		ms.speculator.Cancel()
	}

	ms.mu.Lock()
	ms.utteranceSeq++
	seq := ms.utteranceSeq
	ms.state = StateProcessing
	ms.mu.Unlock()

	go ms.processUtterance(audioData, duration, seq)
}

func (ms *ManagedStream) processUtterance(audioData []byte, duration time.Duration, seq int) {
	ctx, cancel := context.WithTimeout(ms.ctx, 15*time.Second)
	defer cancel()

	// Skip STT entirely if a newer utterance already superseded this one.
	ms.mu.Lock()
	currentSeq := ms.utteranceSeq
	ms.mu.Unlock()
	if currentSeq > seq {
		ms.logger.Info("Skipping STT for superseded utterance", "seq", seq, "currentSeq", currentSeq)
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		return
	}

	ms.sttStartTime = time.Now()

	result, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	if err != nil {
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Transcription error: %v", err))
		}
		return
	}

	ms.sttEndTime = time.Now()
	ms.lastNoSpeechProb = result.NoSpeechProb

	if ms.isLikelyNoise(result, duration) {
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		ms.emit(BotResumed, nil)
		return
	}

	transcript := strings.TrimSpace(result.Text)
	if transcript == "" {
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		return
	}

	ms.lastUserText = transcript

	if ms.userProfile.HasBaseline() {
		wc := countWords(transcript)
		ms.userProfile.RecordUtterance(wc, int(duration.Milliseconds()), 0)
	}

	ms.emit(TranscriptFinal, transcript)
	ms.session.AddMessage("user", transcript)

	// If a newer utterance already arrived, skip LLM — the newest
	// utterance's pipeline will see all accumulated context.
	ms.mu.Lock()
	currentSeq = ms.utteranceSeq
	ms.mu.Unlock()
	if currentSeq > seq {
		ms.logger.Info("Skipping LLM for older utterance",
			"seq", seq, "currentSeq", currentSeq)
		return
	}

	ms.runLLMAndTTS(ctx, transcript)
}

func (ms *ManagedStream) runLLMAndTTS(ctx context.Context, transcript string) {
	rCtx, rCancel := context.WithCancel(ctx)

	ms.mu.Lock()
	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
	}
	ms.pipelineCancel = rCancel
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()

	defer rCancel()

	ms.emitWithGen(BotThinking, nil, gen)
	ms.llmStartTime = time.Now()

	if sProvider, ok := ms.orch.llm.(StreamingLLMProvider); ok {
		ms.runStreamingLLM(rCtx, sProvider, gen)
		return
	}

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	if err != nil {
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.llmEndTime = time.Now()
	ms.session.AddMessage("assistant", response)
	ms.emit(BotResponse, response)

	ms.speakText(rCtx, response, gen)
}




func (ms *ManagedStream) speakText(ctx context.Context, text string, gen int) {
	if ms.prosody != nil {
		pr := ms.prosody.ProcessText(text)
		text = applyProsodyText(pr)
		defer ms.prosody.UpdateContext(text, pr.EstimatedMs)
	}

	if ms.userProfile.HasBaseline() {
		rate := ms.userProfile.GetSuggestedSpeechRate()
		ms.orch.SetTTSRate(rate)
	}

	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	ms.mu.Lock()
	ms.ttsCancel = sCancel
	ms.botSpeakStart = time.Now()
	ms.ttsStartTime = ms.botSpeakStart
	ms.state = StateSpeaking
	ms.mu.Unlock()

	ms.emitWithGen(BotSpeaking, nil, gen)

	isStreaming := ms.orch.GetProviders()["tts"] == "deepgram"

	jitterMs := 0
	if !isStreaming {
		if env := os.Getenv("JITTER_BUFFER_MS"); env != "" {
			if v, err := strconv.Atoi(env); err == nil && v >= 0 {
				jitterMs = v
			}
		}
	}

	frameSize := int(float64(ms.playbackRate) * 0.06) * 2
	if frameSize <= 0 {
		frameSize = 5292
	}
	jitterTarget := int(float64(ms.playbackRate)*float64(jitterMs)/1000.0) * 2
	var jitterBuf []byte
	var started bool

	ms.ttsFirstChunkTime = time.Now()

	// Serialize TTS operations to prevent concurrent WS frame corruption.
	// Only one StreamSynthesize call per session at a time.
	ms.ttsMu.Lock()
	err := ms.orch.SynthesizeStream(sCtx, text,
		ms.session.GetCurrentVoice(),
		ms.session.GetCurrentLanguage(),
		func(chunk []byte) error {
			ms.lastAudioSentAt = time.Now()

			if ms.ttsFirstChunkTime.IsZero() {
				ms.ttsFirstChunkTime = time.Now()
			}

			if isStreaming {
				ms.emitFrames(chunk, frameSize, gen)
				return nil
			}

			if !started {
				jitterBuf = append(jitterBuf, chunk...)
				if len(jitterBuf) >= jitterTarget {
					started = true
					ms.emitFrames(jitterBuf, frameSize, gen)
					jitterBuf = nil
				}
				return nil
			}

			ms.emitFrames(chunk, frameSize, gen)
			return nil
		},
	)
	ms.ttsMu.Unlock()

	if !started && len(jitterBuf) > 0 {
		ms.emitFrames(jitterBuf, frameSize, gen)
	}

	if err != nil && sCtx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.state = StateIdle
	ms.ttsCancel = nil
	ms.ttsEndTime = time.Now()
	ms.mu.Unlock()
}

func (ms *ManagedStream) emitFrames(data []byte, frameSize, gen int) {
	for i := 0; i < len(data); i += frameSize {
		end := i + frameSize
		if end > len(data) {
			end = len(data)
		}
		c := make([]byte, end-i)
		copy(c, data[i:end])
		ms.emitWithGen(AudioChunk, c, gen)
	}
}

func (ms *ManagedStream) handleInterrupt() {
	ms.cancelPipeline()

	ms.mu.Lock()
	oldState := ms.state
	ms.state = StateInterrupted
	ms.mu.Unlock()

	if oldState == StateSpeaking || oldState == StateProcessing {
		ms.drainAudioChunks()
		ms.mu.Lock()
		gen := ms.payloadGen
		ms.mu.Unlock()
		ms.emitWithGen(Interrupted, nil, gen)
	}
}

func (ms *ManagedStream) cancelPipeline() {
	ms.mu.Lock()

	// Abort TTS while still holding ms.mu to prevent a new utterance
	// from acquiring a connection (via speakText → StreamSynthesize)
	// before we close the old one. Without this guard, Abort() can
	// close a connection that a concurrent utterance just opened,
	// causing "received unknown opcode" frame corruption.
	if ms.orch != nil && ms.orch.tts != nil {
		ms.orch.tts.Abort()
	}

	pCancel := ms.pipelineCancel
	tCancel := ms.ttsCancel
	ms.pipelineCancel = nil
	ms.ttsCancel = nil
	ms.mu.Unlock()

	if pCancel != nil {
		pCancel()
	}
	if tCancel != nil {
		tCancel()
	}
}

func (ms *ManagedStream) drainAudioChunks() {
	deadline := time.Now().Add(100 * time.Millisecond)
	var controlEvents []OrchestratorEvent

	for {
		select {
		case ev := <-ms.events:
			if ev.Type != AudioChunk {
				controlEvents = append(controlEvents, ev)
			}
		default:
			goto DrainDone
		}
		if time.Now().After(deadline) {
			goto DrainDone
		}
	}
DrainDone:
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
		}
	}
}

func (ms *ManagedStream) Interrupt() {
	select {
	case ms.interruptChan <- struct{}{}:
	default:
	}
}

func (ms *ManagedStream) internalInterrupt() {
	ms.handleInterrupt()
}

func (ms *ManagedStream) Write(chunk []byte) error {
	buf := make([]byte, len(chunk))
	copy(buf, chunk)
	select {
	case ms.cmdChan <- buf:
	default:
		ms.logger.Warn("Write dropped audio", "len", len(chunk), "cmdChanFull", true)
	}
	return nil
}

func (ms *ManagedStream) IsVADSpeaking() bool {
	return ms.vadSpeaking
}

func (ms *ManagedStream) isLikelyNoise(result TranscriptionResult, audioDuration time.Duration) bool {
	if result.NoSpeechProb > 0.7 {
		return true
	}
	clean := strings.TrimSpace(result.Text)
	if clean == "" {
		return true
	}
	if audioDuration < 300*time.Millisecond && len(clean) <= 1 {
		return true
	}
	return false
}

func countWords(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Fields(s))
}

type rmsProvider interface {
	LastRMS() float64
}

func (ms *ManagedStream) LastRMS() float64 {
	if ms.vad == nil {
		return 0
	}
	if rms, ok := ms.vad.(rmsProvider); ok {
		return rms.LastRMS()
	}
	return 0
}

func (ms *ManagedStream) IsUserSpeaking() bool {
	return ms.vadSpeaking
}

func (ms *ManagedStream) Events() <-chan OrchestratorEvent {
	return ms.events
}

func (ms *ManagedStream) Close() {
	ms.closeOnce.Do(func() {
		ms.mu.Lock()
		ms.isClosed = true
		ms.mu.Unlock()

		ms.cancelPipeline()
		ms.cancel()

		// Clean up Vela model
		if ms.vela != nil {
			ms.vela.Destroy()
		}

		time.Sleep(10 * time.Millisecond)

		close(ms.events)
	})
}

func (ms *ManagedStream) ExportLastUserAudio() (raw []byte, processed []byte) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.userAudio) == 0 {
		userAudioLen := len(ms.userAudio)
		if userAudioLen == 0 {
			return nil, nil
		}
		rawCopy := make([]byte, userAudioLen)
		copy(rawCopy, ms.userAudio)
		return rawCopy, rawCopy
	}
	rawCopy := make([]byte, len(ms.userAudio))
	copy(rawCopy, ms.userAudio)
	return rawCopy, rawCopy
}

type LatencyBreakdown struct {
	UserToSTT          int64
	UserToSTTStart     int64
	STT                int64
	STT_Internal       int64
	UserToLLM          int64
	LLM                int64
	UserToTTSFirstByte int64
	LLMToTTSFirstByte  int64
	TTSTotal           int64
	BotStartLatency    int64
	UserToPlay         int64
	NoSpeechProb       float64
}

func (ms *ManagedStream) GetLatencyBreakdown() LatencyBreakdown {
	var bd LatencyBreakdown

	ue := ms.userSpeechEnd

	if !ue.IsZero() {
		if !ms.sttEndTime.IsZero() {
			bd.UserToSTT = ms.sttEndTime.Sub(ue).Milliseconds()
		}
		if !ms.sttEndTime.IsZero() {
			bd.STT = ms.sttEndTime.Sub(ms.sttStartTime).Milliseconds()
		}
		if !ms.llmEndTime.IsZero() {
			bd.UserToLLM = ms.llmEndTime.Sub(ue).Milliseconds()
		}
		if !ms.llmEndTime.IsZero() && !ms.llmStartTime.IsZero() {
			bd.LLM = ms.llmEndTime.Sub(ms.llmStartTime).Milliseconds()
		}
		if !ms.ttsFirstChunkTime.IsZero() {
			bd.UserToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ue).Milliseconds()
		}
		if !ms.llmEndTime.IsZero() && !ms.ttsFirstChunkTime.IsZero() {
			bd.LLMToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ms.llmEndTime).Milliseconds()
		}
		if !ms.botSpeakStart.IsZero() {
			bd.BotStartLatency = ms.botSpeakStart.Sub(ue).Milliseconds()
		}
		if !ms.lastAudioSentAt.IsZero() {
			bd.UserToPlay = ms.lastAudioSentAt.Sub(ue).Milliseconds()
		}
	}

	if !ms.ttsStartTime.IsZero() && !ms.ttsEndTime.IsZero() {
		bd.TTSTotal = ms.ttsEndTime.Sub(ms.ttsStartTime).Milliseconds()
	}

	bd.NoSpeechProb = ms.lastNoSpeechProb
	return bd
}

func (ms *ManagedStream) GetLatency() int64 {
	if ms.userSpeechEnd.IsZero() || ms.botSpeakStart.IsZero() {
		return 0
	}
	if ms.botSpeakStart.Before(ms.userSpeechEnd) {
		return 0
	}
	return ms.botSpeakStart.Sub(ms.userSpeechEnd).Milliseconds()
}

func (ms *ManagedStream) GetEndToEndLatency() int64 {
	if ms.userSpeechEnd.IsZero() || ms.lastAudioSentAt.IsZero() {
		return 0
	}
	if ms.lastAudioSentAt.Before(ms.userSpeechEnd) {
		return 0
	}
	return ms.lastAudioSentAt.Sub(ms.userSpeechEnd).Milliseconds()
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
	ms.mu.Lock()
	gen := ms.payloadGen
	ms.mu.Unlock()
	ms.emitWithGen(eventType, data, gen)
}

func (ms *ManagedStream) emitWithGen(eventType EventType, data interface{}, gen int) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()

	select {
	case <-ms.ctx.Done():
		return
	default:
	}

	ms.mu.Lock()
	closed := ms.isClosed
	speaking := ms.state == StateSpeaking

	if eventType == BotSpeaking {
		if gen <= ms.lastBotSpeakGen {
			ms.mu.Unlock()
			return
		}
		ms.lastBotSpeakGen = gen
	}
	ms.mu.Unlock()

	if closed {
		return
	}

	if eventType == AudioChunk && !speaking {
		return
	}

	event := OrchestratorEvent{
		Type:       eventType,
		Data:       data,
		Generation: gen,
	}

	select {
	case ms.events <- event:
	default:
	}
}

func (ms *ManagedStream) emitBackchannel(data []byte) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()

	ms.mu.Lock()
	closed := ms.isClosed
	gen := ms.payloadGen
	ms.mu.Unlock()

	if closed || len(data) == 0 {
		return
	}

	// Apply 50% volume reduction to backchannel audio
	reduced := make([]byte, len(data))
	for i := 0; i < len(data)-1; i += 2 {
		sample := int16(data[i]) | int16(data[i+1])<<8
		sample = int16(float64(sample) * 0.5)
		reduced[i] = byte(sample)
		reduced[i+1] = byte(sample >> 8)
	}

	event := OrchestratorEvent{
		Type:       AudioChunk,
		Data:       reduced,
		Generation: gen,
	}

	select {
	case ms.events <- event:
	default:
	}
}

func (ms *ManagedStream) generateBackchannelClips(o *Orchestrator) {
	voice := VoiceF1
	lang := LanguageEn
	if o != nil && o.config.VoiceStyle != "" {
		voice = o.config.VoiceStyle
	}
	if ms.session != nil && ms.session.CurrentLanguage != "" {
		lang = ms.session.CurrentLanguage
	} else if o != nil && o.config.Language != "" {
		lang = o.config.Language
	}

	var phrases []string
	if lang == LanguageEs {
		phrases = []string{"mhm", "ahá", "sí"}
	} else {
		phrases = []string{"mhm", "uh-huh", "yeah"}
	}
	clips := make([][]byte, 0, len(phrases))

	for _, phrase := range phrases {
		audio, err := o.GenerateSilent(ms.ctx, phrase, voice, lang)
		if err == nil && len(audio) > 100 {
			clips = append(clips, audio)
		}
	}

	if len(clips) > 0 && ms.backch != nil {
		ms.backch.SetClips(clips)
	}
}

func (ms *ManagedStream) updateActivity() {
	ms.lastActivityAt = time.Now()
}

func (ms *ManagedStream) monitorInactivity() {
	ms.mu.Lock()
	timeout := 10 * time.Second
	if ms.orch != nil {
		timeout = ms.orch.config.SilenceTimeout
	}
	ms.mu.Unlock()

	if timeout <= 0 {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ms.ctx.Done():
			return
		case <-ticker.C:
			ms.mu.Lock()
			thinking := ms.state == StateProcessing
			speaking := ms.state == StateSpeaking
			userSpeaking := ms.vadSpeaking
			lastActivity := ms.lastActivityAt
			closed := ms.isClosed
			ms.mu.Unlock()

			if closed {
				return
			}

			if !thinking && !speaking && !userSpeaking {
				if time.Since(lastActivity) > timeout {
					ms.updateActivity()
					go func() {
						ms.mu.Lock()
						if ms.state != StateIdle || ms.vadSpeaking {
							ms.mu.Unlock()
							return
						}
						ms.mu.Unlock()
						ms.runLLMAndTTS(ms.ctx, "[USER_SILENCE_TIMEOUT]")
					}()
				}
			}
		}
	}
}

func applyProsodyText(result prosody.ProsodyResult) string {
	var out string
	for i, m := range result.Markers {
		if m.PauseBefore > 200 && len(out) > 0 {
			out += "... "
		}
		out += m.Text
		out += " "
		if m.PauseAfter > 200 && i < len(result.Markers)-1 {
			out += "... "
		}
	}
	return out
}
