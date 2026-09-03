package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/prosody"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/vela"
)

// byteBufPool recycles byte slices to reduce GC pressure in the audio hot path.
var byteBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

func getByteBuf(size int) []byte {
	bp := byteBufPool.Get().(*[]byte)
	b := *bp
	if cap(b) < size {
		b = make([]byte, size)
	} else {
		b = b[:size]
	}
	return b
}

func putByteBuf(b []byte) {
	if cap(b) > 0 {
		b = b[:0]
		byteBufPool.Put(&b)
	}
}

type StreamState int

const (
	StateIdle StreamState = iota
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
	vela               *vela.Detector
	velaSilenceStart   time.Time // Tracks silence start after speech end
	velaPeakFloorYield float32   // Peak floor_yield during current speech period

	cmdChan       chan []byte
	interruptChan chan struct{}
	state         StreamState

	// confirmationGate: when VAD fires speech end, onVADEnd sets this field
	// to a channel. If new audio arrives in handleAudio before the channel is
	// closed (by the confirmation timer), the user resumed speaking and the
	// pending response is cancelled. Channel is nil when no gate is active.
	confirmationGate     chan struct{}
	confirmationGateOnce sync.Once

	// pendingBargeIn tracks a tentative barge-in: raw VAD fired while the bot
	// was speaking/processing, so audio delivery is already suppressed (state
	// left StateSpeaking), but the underlying TTS/LLM pipeline is deliberately
	// NOT torn down yet. Once STT confirms real speech, confirmBargeInIfPending
	// commits to the interrupt; if it turns out to be noise or too short,
	// resolvePendingBargeIn resumes playback instead of leaving dead air.
	// pendingBargeGen pins this to the response generation active when the
	// tentative mute began, so a resume/confirm can't act on a stale turn.
	pendingBargeIn  bool
	pendingBargeGen int

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
	// isClosed and eventsMu guard ms.events: isClosed is atomic so any hot
	// path can check it cheaply, and eventsMu (deliberately separate from the
	// general-purpose mu below, which is contended by most of the pipeline)
	// serializes the isClosed-recheck-then-send in emit/emitBackchannel/
	// drainAudioChunks against Close()'s close(ms.events) — without that,
	// a goroutine can read isClosed as false, get pre-empted, and send on
	// ms.events after Close() has already closed it (send-on-closed-channel).
	isClosed  atomic.Bool
	eventsMu  sync.Mutex
	closeOnce sync.Once

	sttStartTime      time.Time
	sttEndTime        time.Time
	llmStartTime      time.Time
	llmEndTime        time.Time
	ttsStartTime      time.Time
	ttsFirstChunkTime time.Time
	ttsEndTime        time.Time
	botSpeakStart     time.Time
	lastAudioSentAt   time.Time
	lastNoSpeechProb  float64
	lastActivityAt    time.Time

	// Spoken-truth context tracking: the last assistant response and how much
	// of it was actually synthesized/played before an interruption. On interrupt,
	// the context is truncated to only what the user heard (Pipecat/OpenAI pattern).
	lastResponseText   string
	spokenTextPrefix   string
	spokenTextLocked   bool
	responseChunksSent int

	// Post-interrupt backoff: block bot output for a short window after a
	// barge-in so it doesn't talk over the user (Vapi backoffSeconds pattern).
	interruptedAt time.Time

	// Client-side VAD support
	controlChan chan []byte
	clientVAD   bool

	// Speculative LLM execution during speech
	speculator     *SpeculativeExecutor
	lastSpecAt     time.Time
	speechAudioBuf []byte

	// preSpeechBuf stores the last ~300ms of audio unconditionally, updated BEFORE VAD.
	// Used in onVADStart to prepend speech onset that VAD's confirmation window missed.
	preSpeechBuf *bytes.Buffer

	// Streaming STT: process audio incrementally instead of full buffer
	sttChan       chan []byte
	sttResultChan chan string
	sttStarted    bool
	sttAudioChan  chan<- []byte

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

	// Client-side tool calls: map of callID -> result channels
	clientToolResults   map[string]chan string
	clientToolResultsMu sync.Mutex

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
		vad:             streamVAD,
		playbackRate:    44100,
		inputSampleRate: cfg.SampleRate,
		turnComp:        NewTurnCompletionAnalyzer(),
		userProfile:     prosody.NewUserSpeechProfile(),
		prosody: func() *prosody.AdaptiveProcessor {
			c := prosody.DefaultConfig()
			c.ThinkerMode = true
			c.EmphasisLevel = 0.6
			return prosody.NewAdaptiveProcessor(c)
		}(),
		logger:             logger,
		lastActivityAt:     time.Now(),
		controlChan:        make(chan []byte, 64),
		clientVAD:          cfg.ClientVAD,
		speculator:         NewSpeculativeExecutor(cfg.SpeculativeIntervalMs),
		speechAudioBuf:     make([]byte, 0, 44100),
		speakingRateWindow: make([]float64, 0, 20),
		preSpeechBuf:       bytes.NewBuffer(make([]byte, 0, 300*cfg.SampleRate*2/1000)),
		clientToolResults:  make(map[string]chan string),
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

// SetPlaybackRate configures the playback sample rate used for frame sizing.
// Must be called before audio processing begins (before audioProcessor goroutine).
func (ms *ManagedStream) SetPlaybackRate(rate int) {
	ms.playbackRate = rate
}

func (ms *ManagedStream) audioProcessor() {
	for {
		select {
		case <-ms.ctx.Done():
			return
		case <-ms.interruptChan:
			ms.handleInterrupt()
		case chunk := <-ms.cmdChan:
			func() {
				defer func() {
					if r := recover(); r != nil {
						ms.logger.Error("audioProcessor: recovered panic in handleAudio", "panic", r)
					}
				}()
				ms.handleAudio(chunk)
			}()
		case ctrl := <-ms.controlChan:
			func() {
				defer func() {
					if r := recover(); r != nil {
						ms.logger.Error("audioProcessor: recovered panic in handleControl", "panic", r)
					}
				}()
				ms.handleControl(ctrl)
			}()
		}
	}
}

func (ms *ManagedStream) handleAudio(chunk []byte) {
	// Signal the confirmation gate: new audio arrived during the post-speech-end
	// window. This tells onVADEnd that the user resumed speaking and the pending
	// response should be cancelled.
	ms.mu.Lock()
	if ms.confirmationGate != nil {
		select {
		case <-ms.confirmationGate:
		default:
			close(ms.confirmationGate)
		}
	}
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
		isSpeaking := ms.vadSpeaking
		if isSpeaking {
			ms.userAudio = append(ms.userAudio, chunk...)
			ms.speechAudioBuf = append(ms.speechAudioBuf, chunk...)
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

		// Feed audio to streaming STT for incremental processing
		if ms.sttStarted && ms.sttAudioChan != nil {
			select {
			case ms.sttAudioChan <- chunk:
			default:
			}
		}

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
	audioChunk := chunk
	if ms.inputSampleRate != 16000 {
		audioChunk = resampleTo16k(chunk, ms.inputSampleRate)
	}

	event, err := ms.vela.Process(audioChunk)
	if err != nil {
		ms.logger.Warn("Vela processing error", "error", err)
		return
	}

	wasSpeaking := ms.vadSpeaking
	isSpeaking := ms.vela.IsSpeaking()
	ms.vadSpeaking = isSpeaking

	cfg := ms.orch.GetConfig()

	// Track peak floor_yield during speech period
	if isSpeaking && event.FloorYield > ms.velaPeakFloorYield {
		ms.velaPeakFloorYield = event.FloorYield
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

	// Speech start detection
	if isSpeaking && !wasSpeaking {
		ms.velaPeakFloorYield = 0
		if prevState != StateListening && prevState != StateProcessing {
			ms.onVADStart(prevState)
		}
	}

	// Neural turn end: when VAD drops to silence, check if model detected yield intent
	if wasSpeaking && !isSpeaking && ms.velaPeakFloorYield > 0.5 {
		ms.logger.Info("Vela: neural turn completion", "peak_floor_yield", ms.velaPeakFloorYield)
		ms.onVADEnd(prevState)
		ms.velaPeakFloorYield = 0
		ms.velaSilenceStart = time.Time{}
		return
	}

	// Fallback: silence timer only if model didn't produce a yield signal
	if wasSpeaking && !isSpeaking {
		ms.velaSilenceStart = time.Now()
	}

	if ms.velaSilenceStart != (time.Time{}) && !isSpeaking {
		// Fallback silence threshold: 75ms when Vela neural model didn't detect yield.
		// This is a fast fallback for cases where floor_yield < 0.5. Can be tuned via env var.
		fallbackThreshold := 75 * time.Millisecond
		if envThreshold := os.Getenv("VELA_FALLBACK_SILENCE_MS"); envThreshold != "" {
			if ms, err := strconv.Atoi(envThreshold); err == nil && ms > 0 {
				fallbackThreshold = time.Duration(ms) * time.Millisecond
			}
		}
		if time.Since(ms.velaSilenceStart) >= fallbackThreshold {
			if prevState == StateListening || prevState == StateProcessing {
				ms.onVADEnd(prevState)
				ms.velaSilenceStart = time.Time{}
				return
			}
		}
	}

	if isSpeaking {
		ms.velaSilenceStart = time.Time{}
	}

	if event.InterruptionSafety > cfg.VelaInterruptThreshold && isSpeaking {
		if prevState == StateSpeaking || prevState == StateProcessing {
			// Same tentative-mute pattern as onVADStart: suppress audio via
			// state immediately, defer the destructive cancel to onVADEnd's
			// confirmation so a false-positive neural trigger can resume
			// instead of leaving the caller with dead air.
			ms.mu.Lock()
			ms.state = StateListening
			ms.pendingBargeIn = true
			ms.pendingBargeGen = ms.payloadGen
			ms.mu.Unlock()
			ms.emit(UserSpeaking, nil)
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
	// Cooldown: ignore VAD start if a speech end happened <200ms ago AND the
	// bot hasn't started speaking yet. If the bot is already processing/speaking,
	// allow immediate barge-in — the user didn't actually finish speaking.
	if prevState != StateSpeaking && prevState != StateProcessing {
		if !ms.userSpeechEnd.IsZero() && time.Since(ms.userSpeechEnd) < 200*time.Millisecond {
			ms.logger.Info("VAD start ignored (cooldown)", "since_end_ms", time.Since(ms.userSpeechEnd).Milliseconds())
			return
		}
	}

	ms.userSpeakingSince = time.Now()

	ms.userSpeakingSince = time.Now()

	// Reset tool call counts for a new user turn — prevents the 3-call-per-tool
	// limit from aborting legitimate repeated tool use in long sessions.
	ms.session.ResetToolCallCounts()

	// Start streaming STT session — process audio incrementally
	// This saves ~400ms by not waiting for VAD speech end
	if streamingSTT, ok := ms.orch.stt.(StreamingSTTProvider); ok {
		ms.sttResultChan = make(chan string, 10) // Buffer for partials
		audioChan, err := streamingSTT.StreamTranscribe(ms.ctx, ms.session.GetCurrentLanguage(), func(transcript string, isFinal bool) error {
			// Store partials in channel — processUtterance will read the latest
			select {
			case ms.sttResultChan <- transcript:
			default:
				// Channel full, discard old partial
				select {
				case <-ms.sttResultChan:
				default:
				}
				ms.sttResultChan <- transcript
			}
			return nil
		})
		if err == nil && audioChan != nil {
			ms.sttAudioChan = audioChan
			ms.sttStarted = true
			ms.logger.Info("Streaming STT session started")
		}
	}

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
	if ms.clientVAD {
		ms.vadSpeaking = true
	}
	ms.mu.Unlock()

	if prevState == StateSpeaking || prevState == StateProcessing {
		// Tentative barge-in only: ms.state was already set to StateListening
		// above, which makes emitWithGen's AudioChunk gate suppress outbound
		// audio immediately (as fast as the old cancelPipeline() call was) —
		// but we deliberately do NOT cancel the pipeline here. If this turns
		// out to be a false alarm (noise, too short, or too few words), the
		// still-running TTS goroutine can resume delivering audio with no
		// re-synthesis and no gap in generation. The pipeline is only
		// destructively cancelled once onVADEnd/processUtterance below
		// confirms real speech via confirmBargeInIfPending.
		ms.mu.Lock()
		ms.pendingBargeIn = true
		ms.pendingBargeGen = ms.payloadGen
		ms.mu.Unlock()
		ms.emit(UserSpeaking, nil)
		return
	}

	ms.emit(UserSpeaking, nil)
}

// resolvePendingBargeIn reverts a tentative barge-in that turned out to be a
// false alarm. If the previous turn's pipeline is still alive, playback
// resumes (state goes back to whatever it was actively doing); otherwise it
// falls back to the normal idle reset. No-ops if there is no pending barge-in
// for the current response generation (e.g. it was already confirmed, or a
// newer turn has since started).
func (ms *ManagedStream) resolvePendingBargeIn() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if !ms.pendingBargeIn || ms.pendingBargeGen != ms.payloadGen {
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		return
	}
	ms.pendingBargeIn = false
	switch {
	case ms.ttsCancel != nil:
		ms.state = StateSpeaking
	case ms.pipelineCancel != nil:
		ms.state = StateProcessing
	default:
		ms.state = StateIdle
	}
}

// confirmBargeInIfPending finalizes a tentative barge-in once STT confirms the
// interrupting audio was real speech, running the same cancel + spoken-truth
// truncation + Interrupted-event bookkeeping as handleInterrupt(), but
// synchronously at the point of confirmation rather than depending on a
// separate async re-signal from the transport layer (which is what produced
// the double-cancellation race this replaces). No-ops if there's no pending
// barge-in for the current generation.
func (ms *ManagedStream) confirmBargeInIfPending() {
	ms.mu.Lock()
	pending := ms.pendingBargeIn && ms.pendingBargeGen == ms.payloadGen
	if pending {
		ms.pendingBargeIn = false
	}
	ms.mu.Unlock()
	if pending {
		ms.handleInterrupt()
	}
}

func (ms *ManagedStream) onVADEnd(prevState StreamState) {
	ms.userSpeechEnd = time.Now()
	ms.emit(UserStopped, nil)

	// Finalize streaming STT session
	if ms.sttStarted && ms.sttResultChan != nil {
		close(ms.sttResultChan)
		ms.sttStarted = false
	}

	ms.mu.Lock()
	if ms.clientVAD {
		ms.vadSpeaking = false
	}
	ms.mu.Unlock()

	if ms.backch != nil {
		ms.backch.UserStarted()
		ms.backch.UserStopped()
	}

	duration := ms.userSpeechEnd.Sub(ms.userSpeakingSince)
	audioData := ms.userAudio
	ms.userAudio = nil

	speechAudio := ms.speechAudioBuf
	ms.speechAudioBuf = make([]byte, 0, 44100)

	// Adaptive VAD: if energy was rising before speech end, the user is likely
	// pausing mid-thought — extend the minimum duration to avoid splitting
	// consecutive sentences across separate turns.
	minDur := 100 * time.Millisecond
	minLen := 80
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
		// Too brief to even bother with STT — if this cut off a tentative
		// barge-in, resume the bot rather than leaving it silent.
		ms.resolvePendingBargeIn()
		return
	}

	// Confirmation gate: after VAD fires speech end, wait a short window
	// for the user to resume speaking. If they do, cancel the pending
	// response and continue listening. This prevents "phantom interrupts"
	// where a brief pause between sentences triggers the bot.
	confirmMs := ms.orch.config.SilenceConfirmationMs
	if confirmMs > 0 {
		gate := make(chan struct{})
		ms.mu.Lock()
		ms.confirmationGate = gate
		ms.mu.Unlock()

		timer := time.NewTimer(time.Duration(confirmMs) * time.Millisecond)
		defer timer.Stop()

		// Block until either: (a) gate closed (new audio = user resumed),
		// (b) timer expires (user truly done), or (c) context cancelled.
		select {
		case <-gate:
			// New audio arrived during confirmation window — user resumed.
			ms.logger.Info("Confirmation gate: user resumed after speech end",
				"confirmMs", confirmMs)
			ms.mu.Lock()
			ms.confirmationGate = nil
			ms.mu.Unlock()
			ms.resolvePendingBargeIn()
			return
		case <-timer.C:
			// Timer expired — user is done, proceed with processing.
		case <-ms.ctx.Done():
			ms.mu.Lock()
			ms.confirmationGate = nil
			ms.mu.Unlock()
			return
		}

		ms.mu.Lock()
		ms.confirmationGate = nil
		ms.mu.Unlock()
	}

	ms.mu.Lock()
	ms.utteranceSeq++
	seq := ms.utteranceSeq
	ms.state = StateProcessing
	ms.mu.Unlock()

	go ms.processUtterance(audioData, duration, seq)
}

func (ms *ManagedStream) processUtterance(audioData []byte, duration time.Duration, seq int) {
	defer func() {
		if r := recover(); r != nil {
			ms.logger.Error("processUtterance: recovered panic", "panic", r)
			ms.mu.Lock()
			if ms.state != StateInterrupted {
				ms.state = StateIdle
			}
			ms.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(ms.ctx, 15*time.Second)
	defer cancel()

	// Skip STT entirely if a newer utterance already superseded this one.
	ms.mu.Lock()
	currentSeq := ms.utteranceSeq
	ms.mu.Unlock()
	if currentSeq > seq {
		ms.logger.Info("Skipping STT for superseded utterance", "seq", seq, "currentSeq", currentSeq)
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		return
	}

	ms.sttStartTime = time.Now()

	// Read the LATEST streaming partial — don't wait, just grab what's available
	var result TranscriptionResult
	var err error
	if ms.sttResultChan != nil {
		// Drain channel to get the latest partial
		var latestPartial string
		for {
			select {
			case transcript := <-ms.sttResultChan:
				if transcript != "" {
					latestPartial = transcript
				}
			default:
				// No more partials available
				goto gotPartial
			}
		}
	gotPartial:
		if latestPartial != "" {
			result = TranscriptionResult{Text: latestPartial}
			ms.logger.Info("Using streaming STT partial", "text", latestPartial)
		}
		ms.sttResultChan = nil
	}

	// Use batch STT if streaming didn't produce a result (more accurate)
	if result.Text == "" {
		result, err = ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	}
	if err != nil {
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Transcription error: %v", err))
		}
		return
	}

	ms.sttEndTime = time.Now()
	ms.lastNoSpeechProb = result.NoSpeechProb

	if ms.isLikelyNoise(result, duration) {
		// False alarm — if this cut off a tentative barge-in, resume the bot
		// instead of leaving the caller with dead air.
		ms.resolvePendingBargeIn()
		ms.emit(BotResumed, nil)
		return
	}

	transcript := strings.TrimSpace(result.Text)
	if transcript == "" {
		ms.resolvePendingBargeIn()
		return
	}

	// Barge-in confirmation gate: if this utterance tentatively interrupted a
	// still-speaking/processing bot, require at least MinWordsToInterrupt
	// words before committing to the interrupt — short interjections ("uh",
	// "yeah") that don't trip isLikelyNoise still shouldn't cut the bot off.
	// Below the threshold, resume instead of committing.
	ms.mu.Lock()
	pendingBarge := ms.pendingBargeIn && ms.pendingBargeGen == ms.payloadGen
	ms.mu.Unlock()
	if pendingBarge {
		if minWords := ms.orch.config.MinWordsToInterrupt; minWords > 0 && countWords(transcript) < minWords {
			ms.resolvePendingBargeIn()
			return
		}
		// Echo check: there's no acoustic echo cancellation between what the
		// bot is currently speaking and what the mic picks up beyond
		// whatever the client provides — real on browser (WebRTC AEC), but
		// nonexistent for telephony (Telnyx/Twilio), where there's no
		// client-side AEC at all. Without this, the bot's own voice bleeding
		// into the mic gets transcribed, treated as a real barge-in, cuts
		// itself off mid-sentence, and the next turn can trigger the same
		// thing again — a self-interruption loop that looks like the bot
		// restarting/repeating itself over and over.
		ms.mu.Lock()
		currentlySpeaking := ms.lastResponseText
		ms.mu.Unlock()
		if isLikelyEcho(transcript, currentlySpeaking) {
			ms.logger.Info("Barge-in looks like an echo of the bot's own speech, resuming",
				"transcript", transcript)
			ms.resolvePendingBargeIn()
			return
		}
	}
	// Real, sufficient speech — commit to the interrupt now (cancels the old
	// pipeline, truncates spoken-truth context, emits Interrupted). No-op if
	// there was no pending barge-in for this generation.
	ms.confirmBargeInIfPending()

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

	// Check response cache before calling LLM
	if response, audio, ok := ms.checkResponseCache(transcript); ok {
		ms.emit(BotResponse, response)
		if audio != nil {
			frameSize := int(float64(ms.playbackRate)*0.06) * 2
			if frameSize <= 0 {
				frameSize = 5292
			}
			ms.emitFrames(audio, frameSize, 0)
		}
		return
	}

	// Turn-time RAG: if a RAG provider is configured, retrieve relevant context
	// for the user's transcript and inject it into context before the LLM call
	// (LiveKit pattern — avoids extra tool round-trips).
	ms.injectRagContext(transcript)

	ms.runLLMAndTTS(ctx, transcript)

	// Log latency breakdown for observability
	bd := ms.GetLatencyBreakdown()
	ms.logger.Info("utterance_latency",
		"stt_ms", bd.STT,
		"llm_ms", bd.LLM,
		"tts_first_ms", bd.LLMToTTSFirstByte,
		"tts_total_ms", bd.TTSTotal,
		"e2e_ms", bd.UserToPlay,
		"bot_start_ms", bd.BotStartLatency,
		"transcript", transcript,
	)
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
		ms.runStreamingLLM(rCtx, sProvider, gen, transcript)
		return
	}

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	if err != nil {
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	// Non-streaming providers (Anthropic, OpenAI) signal a tool call by
	// returning a "[TOOL_CALLS] <json>" marker string instead of invoking a
	// callback, since they don't implement StreamingLLMProvider. Previously
	// this marker was never checked here — it went straight to speakText, so
	// the caller heard the tool-call JSON read aloud verbatim and the tool
	// itself never ran.
	if calls, isToolCall := parseToolCallMarker(response); isToolCall {
		ms.handleNonStreamingToolCalls(rCtx, gen, transcript, calls)
		return
	}

	ms.llmEndTime = time.Now()
	ms.lastResponseText = response
	ms.spokenTextPrefix = ""
	ms.spokenTextLocked = false
	ms.session.AddMessage("assistant", response)
	ms.emit(BotResponse, response)
	ms.cacheResponse(transcript, response, nil)

	// Full-response TTS (single pass, no sentence pipelining — avoids residual audio on interrupt)
	ms.speakText(rCtx, response, gen)
}

func (ms *ManagedStream) speakText(ctx context.Context, text string, gen int) {
	// Prosody processor: disabled — it modifies text in unpredictable ways
	// (adds filler words, inserts "...", changes pacing) which causes the TTS
	// model to skip or repeat words. Raw LLM text goes directly to TTS.

	// Post-interrupt backoff: if the user just barged in, wait a bit before
	// speaking so we don't talk over them (Vapi backoffSeconds pattern).
	// Measured from the interrupt itself, not from now — the STT+LLM work
	// already done to get here usually covers most or all of this window,
	// so this rarely adds its configured value in full on top.
	backoff := ms.orch.config.PostInterruptBackoff
	ms.mu.Lock()
	sinceInterrupt := time.Since(ms.interruptedAt)
	ms.mu.Unlock()
	if backoff > 0 && sinceInterrupt > 0 && sinceInterrupt < backoff {
		ms.logger.Info("Post-interrupt backoff: delaying speech",
			"since_interrupt_ms", sinceInterrupt.Milliseconds(), "backoff_ms", backoff.Milliseconds())
		time.Sleep(backoff - sinceInterrupt)
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
	// Tracks whatever's actively coming out of the speaker right now — used
	// to detect a barge-in that's actually an echo of the bot's own voice
	// (see isLikelyEcho in processUtterance). In the streaming path this is
	// the current sentence, not the full multi-sentence response, since
	// that's what's actually audible at any given moment.
	ms.lastResponseText = text
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

	frameSize := int(float64(ms.playbackRate)*0.06) * 2
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
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.responseChunksSent++
			// Once the first audio chunk is delivered, mark the spoken prefix as
			// locked — the user has started hearing the response.
			if ms.spokenTextLocked == false && ms.lastResponseText == text {
				ms.spokenTextPrefix = text
				ms.spokenTextLocked = true
			}
			ms.mu.Unlock()

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
	if ms.state != StateInterrupted {
		ms.state = StateIdle
	}
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

	// Spoken-truth context: if the bot was interrupted mid-response, truncate
	// the last assistant message to only the text that was actually spoken.
	// This prevents the model from "remembering" things it never said.
	ms.truncateSpokenContext()

	ms.mu.Lock()
	oldState := ms.state
	ms.state = StateInterrupted
	ms.interruptedAt = time.Now()
	ms.mu.Unlock()

	if oldState == StateSpeaking || oldState == StateProcessing {
		ms.drainAudioChunks()
		ms.mu.Lock()
		gen := ms.payloadGen
		ms.mu.Unlock()
		ms.emitWithGen(Interrupted, nil, gen)
	}
}

// truncateSpokenContext replaces the last assistant message in the session
// context with the portion of the response that was actually spoken, if any.
// This keeps the LLM's understanding aligned with what the user actually heard.
func (ms *ManagedStream) truncateSpokenContext() {
	ms.mu.Lock()
	prefix := ms.spokenTextPrefix
	locked := ms.spokenTextLocked
	chunksSent := ms.responseChunksSent
	ms.mu.Unlock()

	// If no audio chunks were delivered for the current response, the bot was
	// interrupted before speaking anything — remove the assistant message from
	// context so the model doesn't "remember" a response it never gave.
	if chunksSent == 0 {
		ms.removeLastAssistantMessage()
		return
	}

	if prefix == "" || !locked {
		return
	}

	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		ms.removeLastAssistantMessage()
		return
	}

	// Update the last assistant message in context
	ms.session.mu.Lock()
	defer ms.session.mu.Unlock()
	for i := len(ms.session.Context) - 1; i >= 0; i-- {
		msg := &ms.session.Context[i]
		if msg.Role == "assistant" && msg.Content == ms.lastResponseText {
			// Keep only what was actually spoken
			msg.Content = trimmed
			ms.session.LastAssistant = trimmed
			ms.logger.Info("Spoken-truth context truncated",
				"full_len", len(ms.lastResponseText), "spoken_len", len(trimmed))
			break
		}
	}
}

// injectRagContext retrieves relevant knowledge-base context for the user's
// transcript and injects it into the session context before the LLM call.
// This is a no-op unless a RAG provider is configured on the orchestrator.
func (ms *ManagedStream) injectRagContext(transcript string) {
	if ms.orch == nil || ms.orch.rag == nil {
		return
	}
	// Retrieve context asynchronously so it doesn't block the turn
	go func(query string) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		contextText, err := ms.orch.rag.Retrieve(ctx, query)
		if err != nil || contextText == "" {
			return
		}
		// Inject as a system message so the LLM sees it as reference material
		ms.session.AddMessageRaw(Message{
			Role:    "system",
			Content: "[Relevant context: " + contextText + "]",
		})
		ms.logger.Info("RAG context injected", "query_len", len(query), "context_len", len(contextText))
	}(transcript)
}

// removeLastAssistantMessage removes the most recent assistant message from
// context (used when the bot was interrupted before speaking anything).
func (ms *ManagedStream) removeLastAssistantMessage() {
	ms.session.mu.Lock()
	defer ms.session.mu.Unlock()
	for i := len(ms.session.Context) - 1; i >= 0; i-- {
		if ms.session.Context[i].Role == "assistant" {
			ms.session.Context = append(ms.session.Context[:i], ms.session.Context[i+1:]...)
			ms.session.LastAssistant = ""
			ms.logger.Info("Removed unspoken assistant message from context")
			return
		}
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
	// Same eventsMu guard as emitWithGen/emitBackchannel: a concurrent
	// Close() may have closed ms.events between the drain loop above and
	// this resend.
	ms.eventsMu.Lock()
	defer ms.eventsMu.Unlock()
	if ms.isClosed.Load() {
		return
	}
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

// isLikelyEcho reports whether transcript looks like the mic picked up the
// bot's own currently-speaking text rather than the caller actually talking
// over it. There's no acoustic echo cancellation on telephony calls (Telnyx/
// Twilio), and the browser path only gets whatever the client's WebRTC AEC
// manages, so the bot's voice bleeding back into the mic is a real and
// common source of false barge-ins — confirming one cuts the bot off
// mid-sentence, which is what shows up in production as the bot restarting
// or re-saying the same sentence over and over.
func isLikelyEcho(transcript, currentlySpeaking string) bool {
	t := normalizeForEchoCompare(transcript)
	s := normalizeForEchoCompare(currentlySpeaking)
	if t == "" || s == "" {
		return false
	}
	if strings.Contains(s, t) {
		return true
	}
	// STT on a bleed-through echo is often imperfect (clipped audio, mixed
	// with room noise), so also accept near-total word overlap rather than
	// requiring an exact substring match.
	tWords := strings.Fields(t)
	if len(tWords) == 0 {
		return false
	}
	sWords := make(map[string]bool, len(tWords))
	for _, w := range strings.Fields(s) {
		sWords[w] = true
	}
	matched := 0
	for _, w := range tWords {
		if sWords[w] {
			matched++
		}
	}
	return float64(matched)/float64(len(tWords)) >= 0.8
}

func normalizeForEchoCompare(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
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

func (ms *ManagedStream) SubmitToolResult(callID string, result string) {
	ms.clientToolResultsMu.Lock()
	ch, ok := ms.clientToolResults[callID]
	if ok {
		delete(ms.clientToolResults, callID)
	}
	ms.clientToolResultsMu.Unlock()
	if ok {
		select {
		case ch <- result:
		default:
		}
	}
}

func (ms *ManagedStream) RegenerateBackchannelClips(o *Orchestrator) {
	if o == nil || o.tts == nil || ms.backch == nil {
		return
	}
	go func() {
		voice := VoiceF1
		lang := LanguageEn
		if ms.session != nil && ms.session.GetCurrentVoice() != "" {
			voice = ms.session.GetCurrentVoice()
		} else if o != nil && o.config.VoiceStyle != "" {
			voice = o.config.VoiceStyle
		}
		if ms.session != nil && ms.session.CurrentLanguage != "" {
			lang = ms.session.CurrentLanguage
		} else if o != nil && o.config.Language != "" {
			lang = o.config.Language
		}

		phrases := backchannelPhrasesForLang(lang)
		clips := make([][]byte, 0, len(phrases))

		for _, phrase := range phrases {
			audio, err := o.GenerateSilent(ms.ctx, phrase, voice, lang)
			if err == nil && len(audio) > 100 {
				clips = append(clips, audio)
			}
		}

		if len(clips) > 0 {
			ms.backch.SetClips(clips)
		}
	}()
}

// splitSentences splits text on sentence-ending punctuation (.!?)
// while preserving the punctuation. Returns at least one sentence.
func splitSentences(text string) []string {
	var res []string
	var cur strings.Builder
	for _, c := range text {
		cur.WriteRune(c)
		if c == '.' || c == '!' || c == '?' {
			s := strings.TrimSpace(cur.String())
			if s != "" {
				res = append(res, s)
			}
			cur.Reset()
		}
	}
	remaining := strings.TrimSpace(cur.String())
	if remaining != "" {
		res = append(res, remaining)
	}
	if len(res) == 0 {
		res = []string{text}
	}
	return res
}

func (ms *ManagedStream) Close() {
	ms.closeOnce.Do(func() {
		ms.isClosed.Store(true)

		ms.cancelPipeline()
		ms.cancel()

		// Cross-call memory: extract key facts from the conversation so the next
		// session with this user can start with context (Retell/ElevenLabs pattern).
		ms.extractUserMemory()

		// Clean up Vela model
		if ms.vela != nil {
			ms.vela.Destroy()
		}

		time.Sleep(10 * time.Millisecond)

		// Closing under eventsMu (the same lock emit/emitBackchannel/
		// drainAudioChunks hold across their isClosed-recheck-and-send) makes
		// this safe without relying on the sleep above as the only guard: any
		// of those calls that started before this point either finishes its
		// send while holding eventsMu (channel still open) or observes
		// isClosed=true and returns before ever reaching the send.
		ms.eventsMu.Lock()
		close(ms.events)
		ms.eventsMu.Unlock()
	})
}

// extractUserMemory runs a cheap LLM extraction over the conversation to capture
// key facts (name, preferences, identifiers) for cross-call memory. Non-blocking.
func (ms *ManagedStream) extractUserMemory() {
	if ms.orch == nil || ms.orch.llm == nil {
		return
	}
	messages := ms.session.GetContextCopy()
	if len(messages) < 2 {
		return
	}

	// Build a compact transcript for the extraction call
	var sb strings.Builder
	for _, msg := range messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			content := msg.Content
			if len(content) > 300 {
				content = content[:300] + "..."
			}
			sb.WriteString(msg.Role + ": " + content + "\n")
		}
	}

	go func(transcript string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		prompt := "Extract key facts about this user from the conversation transcript. " +
			"Return a concise list of facts: name, preferences, identifiers, or important context. " +
			"Format as plain text, max 5 lines.\n\nTranscript:\n" + transcript

		extractionMessages := []Message{
			{Role: "system", Content: "You extract structured user facts from conversations. Be concise and factual."},
			{Role: "user", Content: prompt},
		}
		facts, err := ms.orch.llm.Complete(ctx, extractionMessages, nil)
		if err != nil || facts == "" {
			return
		}

		// Store the extracted facts in the session for use by the next session
		ms.session.mu.Lock()
		ms.session.UserMemory = strings.TrimSpace(facts)
		ms.session.mu.Unlock()
		ms.logger.Info("Cross-call memory extracted", "facts_len", len(facts))
	}(sb.String())
}

func (ms *ManagedStream) ExportLastUserAudio() (raw []byte, processed []byte) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.userAudio) == 0 {
		return nil, nil
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

	if ms.isClosed.Load() {
		return
	}

	ms.mu.Lock()
	speaking := ms.state == StateSpeaking
	if eventType == BotSpeaking {
		if gen <= ms.lastBotSpeakGen {
			ms.mu.Unlock()
			return
		}
		ms.lastBotSpeakGen = gen
	}
	ms.mu.Unlock()

	if eventType == AudioChunk && !speaking {
		return
	}

	event := OrchestratorEvent{
		Type:       eventType,
		Data:       data,
		Generation: gen,
	}

	// eventsMu (not the general-purpose ms.mu above) serializes this send
	// against Close()'s close(ms.events) — see the eventsMu field comment.
	// Re-checking isClosed here (not just the cheap check above) closes the
	// actual race: without a shared lock across "check" and "send", a
	// goroutine can observe isClosed=false, get pre-empted, and send after
	// Close() has since closed the channel.
	ms.eventsMu.Lock()
	defer ms.eventsMu.Unlock()
	if ms.isClosed.Load() {
		return
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

	if len(data) == 0 {
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

	if ms.isClosed.Load() {
		return
	}

	ms.mu.Lock()
	gen := ms.payloadGen
	ms.mu.Unlock()

	event := OrchestratorEvent{
		Type:       AudioChunk,
		Data:       reduced,
		Generation: gen,
	}

	// eventsMu serializes this send against Close()'s close(ms.events) — see
	// the eventsMu field comment and emitWithGen.
	ms.eventsMu.Lock()
	defer ms.eventsMu.Unlock()
	if ms.isClosed.Load() {
		return
	}
	select {
	case ms.events <- event:
	default:
		ms.logger.Warn("backchannel drop (channel full)")
	}
}

func backchannelPhrasesForLang(lang Language) []string {
	switch lang {
	case LanguageEs:
		return []string{"mhm", "ahá", "sí"}
	case LanguageFr:
		return []string{"mhm", "uh-huh", "oui"}
	case LanguageDe:
		return []string{"mhm", "aha", "ja"}
	case LanguageIt:
		return []string{"mhm", "uh-huh", "sì"}
	case LanguagePt:
		return []string{"mhm", "uh-huh", "sim"}
	case LanguageJa:
		return []string{"un", "hai", "ee"}
	case LanguageKo:
		return []string{"eum", "eo", "ne"}
	case LanguageZh:
		return []string{"en", "a", "shi"}
	case LanguageAr:
		return []string{"hmm", "ah", "naam"}
	case LanguageBg:
		return []string{"mhm", "ahah", "da"}
	case LanguageHr:
		return []string{"mhm", "aha", "da"}
	case LanguageCs:
		return []string{"mhm", "aha", "ano"}
	case LanguageDa:
		return []string{"mhm", "naa", "ja"}
	case LanguageNl:
		return []string{"mhm", "uh-huh", "ja"}
	case LanguageEt:
		return []string{"mhm", "ahah", "jah"}
	case LanguageFi:
		return []string{"mhm", "ahaa", "niin"}
	case LanguageEl:
		return []string{"mmm", "aha", "ne"}
	case LanguageHi:
		return []string{"hmm", "haan", "accha"}
	case LanguageHu:
		return []string{"mhm", "aha", "igen"}
	case LanguageId:
		return []string{"mhm", "uh-huh", "ya"}
	case LanguageLv:
		return []string{"mhm", "aha", "jaa"}
	case LanguageLt:
		return []string{"mhm", "aha", "taip"}
	case LanguagePl:
		return []string{"mhm", "aha", "tak"}
	case LanguageRo:
		return []string{"mhm", "aha", "da"}
	case LanguageRu:
		return []string{"mhm", "aha", "da"}
	case LanguageSk:
		return []string{"mhm", "aha", "ano"}
	case LanguageSl:
		return []string{"mhm", "aha", "ja"}
	case LanguageSv:
		return []string{"mhm", "uh-huh", "ja"}
	case LanguageTr:
		return []string{"mhm", "hihi", "evet"}
	case LanguageUk:
		return []string{"mhm", "aha", "tak"}
	case LanguageVi:
		return []string{"um", "u", "vang"}
	default:
		return []string{"mhm", "uh-huh", "yeah"}
	}
}

func (ms *ManagedStream) generateBackchannelClips(o *Orchestrator) {
	voice := VoiceF1
	lang := LanguageEn
	if ms.session != nil && ms.session.GetCurrentVoice() != "" {
		voice = ms.session.GetCurrentVoice()
	} else if o != nil && o.config.VoiceStyle != "" {
		voice = o.config.VoiceStyle
	}
	if ms.session != nil && ms.session.CurrentLanguage != "" {
		lang = ms.session.CurrentLanguage
	} else if o != nil && o.config.Language != "" {
		lang = o.config.Language
	}

	phrases := backchannelPhrasesForLang(lang)
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
			ms.mu.Unlock()

			if ms.isClosed.Load() {
				return
			}

			if !thinking && !speaking && !userSpeaking {
				if time.Since(lastActivity) > timeout {
					ms.updateActivity()
					go func() {
						ms.mu.Lock()
						// Also recover from StateInterrupted: a stuck/wedged
						// interrupt should never leave the caller in permanent
						// silence — this is the last-resort net for that case.
						recoverable := ms.state == StateIdle || ms.state == StateInterrupted
						if !recoverable || ms.vadSpeaking {
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
