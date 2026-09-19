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
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/turno"
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

	// Turno dual-channel model: additive, not a replacement for VAD above.
	// Runs continuously (see turno_bargein.go: feedTurno) for two
	// purposes: (1) a VAD shadow comparison against ms.vad, logged only,
	// zero behavioral effect; (2) while a tentative barge-in is open
	// (ms.pendingBargeIn), tracks the peak bargein score for this window so
	// processUtterance can relax (never bypass) the STT MinWordsToInterrupt
	// gate when Turno corroborates a real interruption. It never
	// resolves/dismisses a barge-in on its own, and — since the AEC-Challenge
	// retrain (see types.go: TurnoBargeinAssistThreshold) — it no longer
	// confirms one unilaterally either; that decision stays with the
	// existing STT-based checks. nil when TurnoModelPath isn't configured.
	turno *turno.Runtime
	// turnoTurn is a second, independent Turno instance used ONLY to record
	// the TurnState/Horizon heads. It gates nothing; its VAD and bargein
	// outputs are discarded. See TurnoTurnModelPath for why the gating model
	// can't supply these (dead horizon head).
	turnoTurn               *turno.Runtime
	turnoBargeinAssistThr   float32
	turnoBargeinWordsRelief int
	turnoBargeinPeakScore   float32 // max bargein score seen during the current pendingBargeGen window
	turnoNearAccum          []byte  // <320-sample leftover near-channel bytes, 16kHz PCM16
	turnoVadDiagFrames      int     // frame counter gating the periodic VAD-shadow log line

	// Latest turn-completion heads, captured per frame by feedTurno and read
	// once per utterance by processUtterance's mid-thought guard. These are
	// the two heads the model computes on every frame but nothing consumed:
	// TurnState is the prosodic analogue of turnComp.IsLikelyComplete's
	// regex, and Horizon predicts end-of-turn 200/500/800ms ahead. Recorded
	// in shadow so the lexical gate can be scored against them on real
	// traffic before either is trusted to gate anything.
	turnoLastTurnState   [4]float32
	turnoLastHorizon     [3]float32
	turnoLastTurnLabel   string
	turnoTurnStateFrames int    // frames captured since the last utterance, 0 = no Turno audio seen
	farEndBuf            []byte // ring of the bot's own recent outgoing audio, resampled to 16kHz PCM16
	farEndMu             sync.Mutex

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

	// Audio produced while a TENTATIVE barge-in is muting playback.
	//
	// The design above promises that a rejected barge-in can "resume delivering
	// audio with no re-synthesis and no gap in generation". That was only ever
	// true for audio not yet generated: emitWithGen dropped every frame that
	// arrived while the stream sat in StateListening, so by the time the
	// barge-in was rejected the frames were gone and resuming produced silence.
	//
	// The synthesiser runs far faster than playback (RTF ~0.4-0.7), so a short
	// reply is generated almost entirely inside the tentative window — measured
	// on production, a 1.88s reply was produced and discarded in full while a
	// spurious VAD start held the gate shut. The caller got a turn with a
	// transcript, a "speaking" status, a healthy [versa] synthesis line and no
	// audio whatsoever.
	//
	// So a tentative mute now holds frames rather than dropping them. Rejecting
	// the barge-in flushes them and playback really does resume; confirming it
	// discards them, which is what a real interruption wants anyway.
	heldAudio      [][]byte
	heldAudioGen   int
	heldAudioBytes int

	userAudio []byte

	userSpeakingSince time.Time
	userSpeechEnd     time.Time
	lastUserText      string

	vadSpeaking   bool
	vadDiagChunks int

	pipelineCancel context.CancelFunc
	// pipelineCtx is the context pipelineCancel cancels. A multi-sentence
	// response is synthesized one speakText() call per sentence, and each
	// call resets ms.state to StateIdle on its own completion — so between
	// sentences, ms.state genuinely reads Idle even though the turn as a
	// whole is still in flight. pipelineCtx stays un-Done for the entire
	// turn (every entry point that sets pipelineCancel also defers its
	// cancel, firing only on real interruption or the turn's true, final
	// completion), so checking pipelineCtx.Err() is the reliable "is a turn
	// actually still active" signal — unlike ms.state, it isn't blind to the
	// gaps between sentences. See handleInterrupt.
	pipelineCtx context.Context
	ttsCancel   context.CancelFunc

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

	sttStartTime time.Time
	sttEndTime   time.Time
	// sttSpeculative records whether this turn's transcript came from the
	// pass launched during the VAD hangover rather than a blocking call after
	// it. Logged per turn so the win is visible and a regression in the accept
	// rate is noticeable.
	sttSpeculative bool
	specSTT        specSTT
	// lastVoicedAt is the last frame on which the VAD saw speech. The VAD
	// hangover sits between this and userSpeechEnd.
	lastVoicedAt time.Time
	// discardedMs accumulates time spent on responses that were generated and
	// then thrown away this turn — the barge-in discard path. Without it that
	// work landed in unaccounted_ms and looked like an unexplained stall: one
	// turn reported 3966ms with 3002ms unaccounted, which was not a wait at
	// all but a response binned and redone.
	discardedMs int64
	// discardStart marks when the work that is about to be discarded began.
	discardStart time.Time
	// Sub-stages of turn_gate_ms (sttEnd -> llmStart). That span measured 10-12
	// SECONDS on two turns of a live call while every component in it is
	// individually bounded — the confirmation gate at 350ms, the speculative
	// await at 4s, RAG async, telemetry non-blocking. Guessing which one is
	// lying is exactly what this log line exists to stop, so the span is split
	// until it accounts for itself.
	confirmWaitMs int64
	specAwaitMs   int64
	// Checkpoints across the sttEnd -> llmStart span, so a large value points
	// at a line rather than a region. Measured in ms from sttEndTime.
	ckShadowMs int64 // through the noise checks and the Turno shadow block
	ckGateMs   int64 // through the confirmation gate
	ckBargeMs  int64 // through the echo check and barge-in resolution
	ckCacheMs  int64 // through the response cache and RAG injection
	// lastDropLogGen rate-limits the dropped-audio warning to one line per
	// response rather than one per frame.
	lastDropLogGen int
	// One "channel full" line per generation, same rate-limiting rationale as
	// lastDropLogGen above.
	lastFullLogGen    int
	llmStartTime      time.Time
	llmEndTime        time.Time
	ttsStartTime      time.Time
	ttsFirstChunkTime time.Time
	// Audio for the opening segment, rendered during the hangover. See prerender.go.
	prerender        prerendered
	ttsEndTime       time.Time
	botSpeakStart    time.Time
	lastAudioSentAt  time.Time
	lastNoSpeechProb float64
	lastActivityAt   time.Time

	// silenceNudgeSent gates monitorInactivity's silence-timeout reprompt to
	// at most once per idle stretch — it used to have no such gate and would
	// refire every ~10s indefinitely while the user stayed silent, each time
	// asking the LLM for a fresh paraphrase of "are you there", producing an
	// unbounded loop of slightly-reworded nudges instead of one prompt
	// followed by real silence. Cleared the moment real speech starts again
	// (onVADStart), so a later genuine silence still gets its own one nudge.
	silenceNudgeSent bool

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

	// Fast pause-trigger for speculation: independent of, and much quicker
	// than, the main VAD's hangover (which deliberately holds "still
	// speaking" through brief gaps). lastRawEnergyAt is stamped on every
	// chunk whose raw PCM RMS crosses pauseEnergyThreshold, regardless of
	// what the hangover-smoothed VAD currently reports; once ~100ms passes
	// with no such chunk, mid-utterance, that's this stream's guess that
	// the user might be done — worth a speculative shot even though the
	// real VAD won't confirm anything for a few hundred more ms.
	// specTriggeredForRun avoids re-firing every chunk through the same
	// quiet stretch; cleared the moment energy resumes, so a later pause
	// in the same utterance gets its own attempt.
	lastRawEnergyAt     time.Time
	specTriggeredForRun bool

	// preSpeechBuf stores the last ~300ms of audio unconditionally, updated BEFORE VAD.
	// Used in onVADStart to prepend speech onset that VAD's confirmation window missed.
	preSpeechBuf *bytes.Buffer

	// Streaming STT: process audio incrementally instead of full buffer
	sttChan       chan []byte
	sttResultChan chan string
	sttStarted    bool
	sttAudioChan  chan<- []byte

	// transportReady is closed by the embedding transport (web WS handler,
	// Telnyx/Twilio media handler) the moment it can deliver bot audio —
	// e.g. once the stream ID is known. The FirstSpeakerBot greeting waits
	// on this gate instead of a fixed sleep: no dead air when the
	// transport is already up, no dropped greeting audio when it isn't.
	transportReadyMu sync.Once
	transportReady   chan struct{}

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
		transportReady:  make(chan struct{}),
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

	// Initialize Turno if configured. This is additive: VAD shadow
	// logging never affects behavior, and the barge-in assist only ever
	// relaxes a gate the STT-confirmation path would reach anyway — so a
	// load failure just leaves both disabled (logged, not fatal).
	if cfg.TurnoModelPath != "" {
		if _, err := os.Stat(cfg.TurnoModelPath); err == nil {
			g, err := turno.NewRuntime(cfg.TurnoModelPath)
			if err != nil {
				logger.Warn("failed to load Turno model, disabled", "error", err)
			} else {
				ms.turno = g
				ms.turnoBargeinAssistThr = cfg.TurnoBargeinAssistThreshold
				ms.turnoBargeinWordsRelief = cfg.TurnoBargeinAssistWordsRelief
				logger.Info("Turno loaded (VAD shadow + barge-in assist)", "model", cfg.TurnoModelPath)
			}
		} else {
			logger.Warn("Turno model file not found, disabled", "path", cfg.TurnoModelPath)
		}
	}

	// Second Turno instance for the turn-completion shadow only. Loaded
	// independently of the gating instance above and never consulted by any
	// gate — if this fails, ms.turnoTurn stays nil and the only consequence
	// is that the shadow log line is absent.
	if cfg.TurnoTurnModelPath != "" && ms.turno != nil {
		if _, err := os.Stat(cfg.TurnoTurnModelPath); err == nil {
			g, err := turno.NewRuntime(cfg.TurnoTurnModelPath)
			if err != nil {
				logger.Warn("failed to load Turno turn-completion model, shadow disabled", "error", err)
			} else {
				ms.turnoTurn = g
				logger.Info("Turno turn-completion shadow loaded (TurnState/Horizon only)",
					"model", cfg.TurnoTurnModelPath)
			}
		} else {
			logger.Warn("Turno turn-completion model not found, shadow disabled",
				"path", cfg.TurnoTurnModelPath)
		}
	}

	if cfg.ResponseCaching {
		ms.responseCache = NewResponseCache(5*time.Minute, 100)
	}

	if cfg.SpeculativeLLM && ms.speculator != nil {
		ms.speculator.SetOnPartial(func(partial string) {
			ms.emit(TranscriptPartial, partial)
		})
		if cfg.SpeculativePrerender {
			// Render the opening segment's audio as soon as the speculative reply exists, which is
			// inside the VAD hangover — so on a hit there is nothing left to synthesise when the
			// turn is confirmed. See prerender.go.
			ms.speculator.SetOnResponse(ms.prerenderFirstSegment)
		}
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
		// Signal-driven wait: fire as soon as the transport can deliver audio.
		go func() {
			if ms.waitTransportReady(3*time.Second) != nil || ms.ctx.Err() != nil {
				return
			}

			// A configured opening is spoken verbatim, with no LLM call: the
			// caller hears exactly what was set, and the call starts a full
			// LLM round-trip sooner.
			msg, instr := resolveOpening(o.config)
			if msg != "" {
				ms.mu.Lock()
				ms.state = StateSpeaking
				gen := ms.payloadGen
				ms.mu.Unlock()
				ms.session.AddMessage("assistant", msg)
				ms.speakText(ms.ctx, msg, gen)
				return
			}

			// No verbatim opening, so the recording notice (if any) is spoken
			// on its own before the model takes its first turn. It still has to
			// come first: the disclosure is only worth anything if it precedes
			// the conversation it is disclosing.
			if notice := strings.TrimSpace(o.config.RecordingNotice); notice != "" {
				ms.mu.Lock()
				ms.state = StateSpeaking
				gen := ms.payloadGen
				ms.mu.Unlock()
				ms.session.AddMessage("assistant", notice)
				ms.speakText(ms.ctx, notice, gen)
			}

			ms.session.AddMessage("user", instr)
			ms.runLLMAndTTS(ms.ctx, "")
		}()
	}

	return ms
}

// SetPlaybackRate configures the playback sample rate used for frame sizing.
// Must be called before audio processing begins (before audioProcessor goroutine).
func (ms *ManagedStream) SetPlaybackRate(rate int) {
	ms.playbackRate = rate
}

// NotifyTransportReady signals that the embedding transport can now deliver
// bot audio (stream ID known). Closes the gate exactly once; safe to call
// multiple times. Transports that are ready immediately (e.g. web
// WebSocket speakers) should call this as soon as the session is wired.
func (ms *ManagedStream) NotifyTransportReady() {
	ms.transportReadyMu.Do(func() {
		close(ms.transportReady)
	})
}

// waitTransportReady blocks until the transport signals readiness. The
// fallback timeout prevents an indefinite hang if the transport never
// signals; the greeting then proceeds and risks dropping its first chunks
// (same behavior as the old fixed-sleep path, but only in the failure case).
func (ms *ManagedStream) waitTransportReady(timeout time.Duration) error {
	select {
	case <-ms.transportReady:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("transport not ready after %v", timeout)
	case <-ms.ctx.Done():
		return ms.ctx.Err()
	}
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

// closeConfirmationGateIfOpen signals the confirmation gate: the caller has
// just been confirmed as actually SPEAKING (not merely "some audio chunk
// arrived" — silence-period packets flow continuously over the media stream
// too) during the post-speech-end window. This tells onVADEnd the user
// resumed and the pending response should be cancelled. Must only be called
// once a chunk has been VAD-confirmed as speech, never unconditionally
// on every incoming chunk — doing so made the gate close on essentially the
// very next packet regardless of silence, defeating the whole point of the
// confirmation window (see the commit that introduced this gate).
func (ms *ManagedStream) closeConfirmationGateIfOpen() {
	ms.mu.Lock()
	if ms.confirmationGate != nil {
		select {
		case <-ms.confirmationGate:
		default:
			close(ms.confirmationGate)
		}
	}
	ms.mu.Unlock()
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
		isSpeaking := ms.vadSpeaking
		if isSpeaking {
			ms.closeConfirmationGateIfOpen()
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

	if ms.vad == nil {
		return
	}

	event, err := ms.vad.Process(chunk)
	if err != nil {
		ms.logger.Warn("VAD process error", "error", err)
		return
	}

	// Diagnostic: log the raw probability periodically even when no event
	// fires, so a call with zero VADSpeechStart events is distinguishable
	// between "genuinely near-zero probability the whole call" (garbled/
	// wrong-codec audio) and "probability crossed threshold briefly but
	// never sustained minSpeechFrames" — the silent VADProvider interface
	// otherwise gives no visibility into that difference.
	ms.vadDiagChunks++
	if ms.vadDiagChunks%50 == 0 {
		if p, ok := ms.vad.(interface{ LastProbability() float64 }); ok {
			ms.logger.Info("VAD diag", "lastProbability", p.LastProbability(), "chunks", ms.vadDiagChunks)
		}
	}

	isSpeaking := ms.vad.IsSpeaking()
	ms.vadSpeaking = isSpeaking

	if ms.turno != nil {
		audioChunk16k := chunk
		if ms.inputSampleRate != 16000 {
			audioChunk16k = resampleTo16k(chunk, ms.inputSampleRate)
		}
		ms.feedTurno(audioChunk16k)
	}

	if event != nil && (event.Type == VADSpeechStart || event.Type == VADSpeechEnd || event.Type == VADSpeechPotential) {
		ms.logger.Info("VAD event",
			"type", event.Type,
			"state", state)
	}

	if isSpeaking {
		ms.closeConfirmationGateIfOpen()
		ms.userAudio = append(ms.userAudio, chunk...)
		ms.speechAudioBuf = append(ms.speechAudioBuf, chunk...)

		// Feed audio to streaming STT for incremental processing
		if ms.sttStarted && ms.sttAudioChan != nil {
			select {
			case ms.sttAudioChan <- chunk:
			default:
			}
		}

		// The last frame that actually carried voice. This — not userSpeechEnd,
		// which is a whole hangover later — is where the caller's own clock
		// starts: they stopped talking, and everything after it is us. Without
		// it the turn log could only measure from a point ~480ms into our own
		// latency budget, which flatters every number in it.
		if sf, ok := ms.vad.(silenceFramesProvider); ok && sf.SilenceFrames() == 0 {
			ms.lastVoicedAt = time.Now()
		}

		// Spend the VAD hangover transcribing rather than waiting. By the time
		// the hangover starts counting, every speech sample is already in the
		// buffer — see speculative_stt.go.
		ms.maybeSpeculateSTT()

		if ms.speculator != nil && ms.orch.config.SpeculativeLLM {
			speechDuration := time.Since(ms.userSpeakingSince)
			if ms.speculator.ShouldSpeculate(speechDuration, ms.lastSpecAt) {
				ms.startSpeculation()
			}
		}
	}

	// Fast pause-trigger, independent of isSpeaking above: raw energy is
	// checked on every chunk regardless of what the hangover-smoothed VAD
	// currently reports, specifically so a brief in-utterance gap can
	// trigger speculation well before the real VAD's ~448ms hangover would
	// ever say the user stopped talking.
	ms.updatePauseSpeculationTrigger(chunk)

	// Turno early end. Speculation above only pre-computes; the hangover is
	// still what commits the turn, so it is the hangover that has to shrink
	// for the caller to hear a reply sooner. The horizon head predicts
	// end-of-turn ~200ms ahead, which is precisely the certainty the hangover
	// is spending time to establish from energy alone.
	ms.maybeArmTurnoEarlyEnd()

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

// maybeArmTurnoEarlyEnd shortens the current utterance's hangover when Turno's
// horizon head is confident the turn is ending.
//
// Only ever shortens, only for the utterance in flight, and only while the
// caller is actually speaking — arming during silence would let a stale frame
// clip the very next utterance. Every downstream guard still runs, so the
// worst case is answering a shade early, not talking over someone.
func (ms *ManagedStream) maybeArmTurnoEarlyEnd() {
	thr := ms.orch.config.TurnoEarlyEndThreshold
	if thr <= 0 || ms.turnoTurn == nil || ms.turnoTurnStateFrames == 0 {
		return
	}
	if !ms.vadSpeaking {
		return
	}
	// Horizon[0] is the 200ms-ahead prediction — the shortest and most
	// confident of the three, and the only one worth acting on this late.
	if ms.turnoLastHorizon[0] < thr {
		return
	}

	earlyMs := ms.orch.config.TurnoEarlyEndMs
	if earlyMs <= 0 {
		earlyMs = 200
	}
	if earlyMs < turnoEarlyEndMinMs {
		earlyMs = turnoEarlyEndMinMs
	}

	if v, ok := ms.vad.(interface{ ArmEarlyEnd(time.Duration) }); ok {
		v.ArmEarlyEnd(time.Duration(earlyMs) * time.Millisecond)
	}
}

// turnoEarlyEndMinMs is the floor on the shortened hangover. Below roughly
// this, normal within-sentence breathing pauses start ending turns, which is
// the failure the hangover exists to prevent in the first place.
const turnoEarlyEndMinMs = 150

// logTurnLatency emits one structured line per turn with the stage breakdown
// behind time-to-first-audio.
//
// Every timestamp it reads already existed and nothing consumed them, so the
// system could not answer "which stage is slow" — the documented latency
// budget drifted badly out of date (it still credited the LLM with 550ms when
// the provider had changed and measures ~40ms) and nobody could tell, because
// there was no per-turn ground truth to contradict it. The Prometheus
// VoiceAgentTTFB histogram beside it is declared and never observed, so it
// emits nothing at all.
//
// A log line rather than a metric on purpose: it survives pod restarts in the
// log aggregator, carries the full breakdown rather than one number, and needs
// no scrape target to be useful.
func (ms *ManagedStream) logTurnLatency() {
	end := ms.userSpeechEnd
	first := ms.ttsFirstChunkTime
	if end.IsZero() || first.IsZero() || first.Before(end) {
		return // bot-initiated turn, or clocks that make the split meaningless
	}

	// The number that matters is the one the caller experiences: from the last
	// frame on which they were actually speaking, to the first byte of audio
	// leaving us. Everything else in this line is a component of it.
	//
	// ttfa_ms used to be the headline, and it is anchored at userSpeechEnd —
	// which is a full VAD hangover (480ms in production) after the caller
	// stopped. That silently excluded the single largest fixed cost in the
	// pipeline from the number we were optimising against. Both are logged now,
	// but e2e_ms is the real one.
	voiced := ms.lastVoicedAt
	hangover := stageMs(voiced, end)
	e2e := int64(-1)
	if !voiced.IsZero() && !first.Before(voiced) {
		e2e = first.Sub(voiced).Milliseconds()
	}

	ttfa := first.Sub(end).Milliseconds()
	sttQueue := stageMs(end, ms.sttStartTime)
	stt := stageMs(ms.sttStartTime, ms.sttEndTime)
	gate := stageMs(ms.sttEndTime, ms.llmStartTime)
	llm := stageMs(ms.llmStartTime, ms.llmEndTime)
	llmToTTS := stageMs(ms.llmEndTime, ms.ttsStartTime)
	ttsFirst := stageMs(ms.ttsStartTime, first)

	// Work that was done and then thrown away — a response generated for a turn
	// the caller then talked over. Real elapsed time that is not attributable
	// to any stage of the response we finally played, so it is named rather
	// than left to swell unaccounted_ms and read as a mystery stall.
	discarded := ms.discardedMs

	// Whatever the named stages still fail to explain. On a healthy turn this
	// should be single-digit milliseconds; anything larger means a stage is
	// missing from this list, not that the turn was slow for free.
	unaccounted := e2e
	if unaccounted < 0 {
		unaccounted = ttfa
	}
	for _, d := range []int64{hangover, sttQueue, stt, gate, llm, llmToTTS, ttsFirst, discarded} {
		if d > 0 {
			unaccounted -= d
		}
	}

	// turn_gate_ms split into its parts, so a large value names its own cause
	// instead of being a span nobody can attribute.
	ms.mu.Lock()
	confirmWait, specAwait := ms.confirmWaitMs, ms.specAwaitMs
	ckShadow, ckGate, ckBarge, ckCache := ms.ckShadowMs, ms.ckGateMs, ms.ckBargeMs, ms.ckCacheMs
	ms.mu.Unlock()
	gateOther := gate
	if gateOther > 0 {
		gateOther -= confirmWait + specAwait
		if gateOther < 0 {
			gateOther = 0
		}
	}

	ms.logger.Info("turn latency",
		// The caller's clock: last voiced frame -> first audio out.
		"e2e_ms", e2e,
		// Silence the VAD required before it would call the turn over.
		"hangover_ms", hangover,
		"stt_queue_ms", sttQueue,
		"stt_ms", stt,
		// Whether this turn's transcript came free from the hangover window.
		// When false on a turn that should have qualified, the accept check in
		// specSTT.awaitUsable rejected it — worth knowing, because the
		// difference between true and false here is most of stt_ms.
		"stt_speculative", ms.sttSpeculative,
		"turn_gate_ms", gate,
		// Where turn_gate_ms went: the mid-thought confirmation wait, the
		// speculative-LLM await, and whatever is left over.
		"gate_confirm_ms", confirmWait,
		"gate_spec_await_ms", specAwait,
		"gate_other_ms", gateOther,
		// Cumulative milliseconds from sttEnd to each checkpoint, so a slow
		// span names the step it is stuck on rather than a region of code.
		"ck_shadow_ms", ckShadow,
		"ck_gate_ms", ckGate,
		"ck_barge_ms", ckBarge,
		"ck_cache_ms", ckCache,
		"llm_ms", llm,
		"llm_to_tts_ms", llmToTTS,
		"tts_first_chunk_ms", ttsFirst,
		"discarded_ms", discarded,
		"unaccounted_ms", unaccounted,
		// Kept for continuity with earlier measurements: same clock as before,
		// anchored after the hangover.
		"ttfa_ms", ttfa,
	)
}

// stageMs returns a stage duration in milliseconds, or -1 when either end is
// unset — an explicit "not measured" rather than a zero that reads as instant.
func stageMs(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return -1
	}
	return to.Sub(from).Milliseconds()
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
	ms.mu.Lock()
	ms.silenceNudgeSent = false
	ms.mu.Unlock()
	ms.specTriggeredForRun = false
	ms.lastRawEnergyAt = time.Time{}

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
		// Guard against a duplicate/back-to-back VAD start (e.g. a jittery
		// client sending two vad_speech_start control frames with no
		// intervening end, or a raw-VAD retrigger) leaking the PREVIOUS
		// streaming STT session: without this, ms.sttAudioChan/sttResultChan
		// below would simply be overwritten, orphaning the old session's
		// channels — nothing would ever close them, so the underlying
		// provider goroutine (and its STT connection/state) would run for
		// the rest of the call with no matching real workload, exactly the
		// leak onVADEnd's cleanup below was written to prevent for the
		// normal one-session-per-utterance case. Close it the same way
		// onVADEnd does before replacing it.
		if ms.sttStarted {
			if ms.sttResultChan != nil {
				close(ms.sttResultChan)
			}
			if ms.sttAudioChan != nil {
				close(ms.sttAudioChan)
			}
			ms.sttStarted = false
		}
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
		} else if err != nil {
			ms.logger.Info("Streaming STT unavailable; using final STT provider", "error", err)
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
		ms.turnoBargeinPeakScore = 0
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
	resumedGen := -1
	ms.mu.Lock()
	if !ms.pendingBargeIn {
		// Nothing was tentatively muted (e.g. a short/noisy utterance that
		// never opened a barge-in at all) — safe to normalize back to idle.
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		return
	}
	if ms.pendingBargeGen != ms.payloadGen {
		// A pending barge-in exists, but not for the CURRENT generation: this
		// call is a stale resolve racing in from an already-superseded
		// utterance (e.g. its async processUtterance goroutine finally
		// reaches isLikelyNoise/resolvePendingBargeIn well after a newer
		// turn already started). A newer turn's pipeline may be legitimately
		// StateProcessing/StateSpeaking right now — forcing ms.state to Idle
		// here would clobber that active turn's state out from under it,
		// which makes emitWithGen's AudioChunk gate (state == StateSpeaking)
		// silently drop that turn's real audio. Per the doc comment above,
		// a stale call must be a true no-op: don't touch ms.state at all.
		ms.mu.Unlock()
		return
	}

	// Never resume playback into someone who is still talking.
	//
	// A tentative barge-in is rejected for several reasons that say nothing
	// about whether the caller has stopped: the utterance was under minDur,
	// isLikelyNoise, or it carried fewer than MinWordsToInterrupt words. All
	// of those can fire while the caller is mid-sentence — they remembered
	// something, started again, and the first fragment was simply too short
	// to clear the gate. Resuming there is the worst possible moment: the
	// agent talks straight over a speaking human.
	//
	// If VAD still reports speech, stay muted and leave the pipeline alone.
	// The utterance in flight will resolve this on its own — confirming a
	// real barge-in once enough words arrive, or calling back here once the
	// caller actually stops.
	// The barge-in stays PENDING here on purpose. Clearing it before this early
	// return is what made the callback this comment promises impossible: the next
	// call would take the !pendingBargeIn branch and force StateIdle, ending the
	// response it was supposed to resume, and frames arriving in the meantime
	// would be dropped rather than held.
	if ms.vadSpeaking {
		ms.state = StateListening
		ms.mu.Unlock()
		return
	}

	ms.pendingBargeIn = false

	switch {
	case ms.ttsCancel != nil:
		ms.state = StateSpeaking
		resumedGen = ms.payloadGen
	case ms.pipelineCancel != nil:
		ms.state = StateProcessing
	default:
		ms.state = StateIdle
		// Nothing is left to play into, so nothing should be kept.
		ms.discardHeldAudioLocked()
	}
	ms.mu.Unlock()

	// Emitted outside the lock: this re-enters emitWithGen, which takes ms.mu.
	if resumedGen >= 0 {
		for _, c := range ms.takeHeldAudio(resumedGen) {
			ms.emitWithGen(AudioChunk, c, resumedGen)
		}
	}
}

// confirmBargeInIfPending finalizes a tentative barge-in once STT confirms the
// interrupting audio was real speech, running the same cancel + spoken-truth
// truncation + Interrupted-event bookkeeping as handleInterrupt(), but
// synchronously at the point of confirmation rather than depending on a
// separate async re-signal from the transport layer (which is what produced
// the double-cancellation race this replaces). No-ops if there's no pending
// barge-in for the current generation.
// heldAudioMaxSeconds bounds how much muted playback is kept for a possible resume.
//
// Two different costs sit on either side of this number. Too small and a legitimate
// resume loses the tail of the sentence it was meant to restore. Too large and a
// rejected barge-in dumps a wall of stale audio at a caller who has long since moved
// on, and the memory is held per stream for the whole window. Four seconds is longer
// than any single synthesised segment (maxSpokenSegment caps a segment at 140 chars,
// roughly 7s of speech at worst, but the first frames are what matter for a resume)
// and short enough that a resume still feels like the same sentence continuing.
const heldAudioMaxSeconds = 4

func (ms *ManagedStream) maxHeldAudioBytesLocked() int {
	rate := ms.playbackRate
	if rate <= 0 {
		rate = 44100
	}
	return rate * 2 * heldAudioMaxSeconds
}

// takeHeldAudio removes and returns the frames held during a tentative barge-in.
// Callers must NOT hold ms.mu: emitting the frames re-enters emitWithGen, which
// takes it.
func (ms *ManagedStream) takeHeldAudio(gen int) [][]byte {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.heldAudioGen != gen || len(ms.heldAudio) == 0 {
		return nil
	}
	frames := ms.heldAudio
	ms.heldAudio = nil
	ms.heldAudioBytes = 0
	return frames
}

// discardHeldAudio throws away held frames — the barge-in was real, so the
// response they belong to is not wanted. Safe to call with ms.mu held.
func (ms *ManagedStream) discardHeldAudioLocked() {
	ms.heldAudio = nil
	ms.heldAudioBytes = 0
}

func (ms *ManagedStream) confirmBargeInIfPending() {
	ms.mu.Lock()
	pending := ms.pendingBargeIn && ms.pendingBargeGen == ms.payloadGen
	if pending {
		ms.pendingBargeIn = false
		// The interrupt is real: the muted frames belong to a response the caller
		// has talked over, and playing them now is precisely what a barge-in is
		// meant to prevent.
		ms.discardHeldAudioLocked()
	}
	ms.mu.Unlock()
	if pending {
		ms.handleInterrupt()
	}
}

func (ms *ManagedStream) onVADEnd(prevState StreamState) {
	ms.userSpeechEnd = time.Now()
	// Each caller turn accounts for its own discarded work. Carrying it across
	// turns would charge one turn for a response abandoned on a previous one.
	ms.mu.Lock()
	ms.discardedMs = 0
	ms.discardStart = time.Time{}
	ms.confirmWaitMs = 0
	ms.specAwaitMs = 0
	ms.ckShadowMs = 0
	ms.ckGateMs = 0
	ms.ckBargeMs = 0
	ms.ckCacheMs = 0
	ms.mu.Unlock()
	ms.emit(UserStopped, nil)

	// Finalize streaming STT session. Closing sttAudioChan is what lets
	// StreamTranscribe's goroutine leave its `case data, ok := <-audioChan`
	// loop via the `!ok` branch and free its whisper stream (kv-cache +
	// compute buffers) — previously only sttResultChan was closed here, so
	// every single utterance leaked its streaming STT goroutine and whisper
	// state until the whole call ended (ctx.Done()), compounding CPU/memory
	// use across a call's utterances with no matching real workload.
	if ms.sttStarted {
		if ms.sttResultChan != nil {
			close(ms.sttResultChan)
		}
		if ms.sttAudioChan != nil {
			close(ms.sttAudioChan)
			ms.sttAudioChan = nil
		}
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
		ms.logger.Info("onVADEnd: utterance too brief, skipping processUtterance",
			"duration_ms", duration.Milliseconds(), "audioBytes", len(audioData), "minDur_ms", minDur.Milliseconds(), "minLen", minLen)
		ms.resolvePendingBargeIn()
		return
	}

	// STT+LLM+TTS generation starts immediately — no blocking wait here.
	// "Did the user actually finish talking" is rechecked once, late, right
	// before speakText plays anything (see the vadSpeaking check there):
	// generation itself already takes hundreds of ms to a couple seconds in
	// practice, which is what a fixed pre-generation wait was mostly there
	// to cover, so by the time audio is ready to play we usually already
	// know whether the user resumed. A phantom interrupt (brief pause
	// between sentences) now costs a wasted STT+LLM call instead of wasted
	// wall-clock time on every single turn — worth it for how much this
	// used to add to every response's latency (see SILENCE_CONFIRMATION_MS
	// change tonight). ms.confirmationGate/closeConfirmationGateIfOpen are
	// now unused (nothing sets the gate non-nil anymore) — left in place
	// rather than ripped out mid-incident.
	ms.mu.Lock()
	ms.utteranceSeq++
	seq := ms.utteranceSeq
	ms.state = StateProcessing
	ms.mu.Unlock()

	go ms.processUtterance(audioData, duration, seq)
}

func (ms *ManagedStream) processUtterance(audioData []byte, duration time.Duration, seq int) {
	ms.logger.Info("processUtterance: entered", "seq", seq, "duration_ms", duration.Milliseconds(), "audioBytes", len(audioData))
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

	// A speculative pass launched at the start of the VAD hangover has usually
	// already finished by now, which takes the whole STT stage out of
	// time-to-first-audio. It is only used when the audio appended since the
	// snapshot is short enough to be the hangover's own silence rather than
	// speech the caller resumed with.
	specUsed := false
	if result.Text == "" {
		bytesPerMs := int(ms.inputSampleRate) * 2 / 1000
		if spec, ok, waited := ms.specSTT.awaitUsable(ctx, seq, len(audioData), bytesPerMs, specSTTMaxTailMs()); ok {
			result = spec
			specUsed = true
			ms.logger.Info("Using speculative STT from the VAD hangover",
				"waited_ms", waited.Milliseconds(), "text", result.Text)
		}
	}

	// Use batch STT if streaming didn't produce a result (more accurate)
	if result.Text == "" {
		result, err = ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	}
	ms.specSTT.invalidate()
	ms.sttSpeculative = specUsed
	if err != nil {
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Transcription error: %v", err))
			ms.logger.Warn("Utterance abandoned: transcription failed", "seq", seq, "error", err)
		} else {
			// The silent one, and the expensive one. A cancelled context here
			// emits NOTHING and logged NOTHING, so a turn that entered
			// processUtterance simply stopped existing: no reply, no error to the
			// caller, no line in the log. Traced from production, a failing
			// session read
			//
			//   processUtterance > Utterance discarded as noise > processUtterance > (nothing)
			//
			// against a healthy one ending in "turn latency", and there was no way
			// to tell from the logs which had happened or why. A caller losing a
			// turn must always leave evidence.
			ms.logger.Warn("Utterance abandoned: pipeline cancelled mid-transcription — the caller gets no reply for this turn",
				"seq", seq, "ctx_err", ctx.Err(), "stt_err", err,
				"duration_ms", duration.Milliseconds())
		}
		return
	}

	ms.sttEndTime = time.Now()
	ms.lastNoSpeechProb = result.NoSpeechProb

	if ms.isLikelyNoise(result, duration) {
		// False alarm — if this cut off a tentative barge-in, resume the bot
		// instead of leaving the caller with dead air. Logged with the raw
		// (possibly gibberish) transcript and the scores that drove the
		// call — otherwise a real, quiet utterance getting discarded here
		// is indistinguishable after the fact from actual silence/noise.
		ms.logger.Info("Utterance discarded as noise, resuming bot",
			"transcript", result.Text, "no_speech_prob", result.NoSpeechProb,
			"audio_duration_ms", duration.Milliseconds())
		ms.resolvePendingBargeIn()
		ms.emit(BotResumed, nil)
		return
	}

	transcript := strings.TrimSpace(result.Text)
	// Parakeet emits punctuation but never '?', so a question arrives as a
	// statement and the model answers as though nothing was asked. Restore it
	// before anything downstream reads the text — the lexical gate, the LLM and
	// the stored conversation history all benefit from knowing it was a
	// question. See question_mark.go for why this is lexical and conservative.
	if restored := restoreQuestionMark(transcript, ms.session.GetCurrentLanguage()); restored != transcript {
		ms.logger.Info("Restored question mark the recogniser omitted",
			"before", transcript, "after", restored)
		transcript = restored
	}
	if transcript == "" {
		ms.logger.Info("Utterance discarded: empty transcript, resuming bot",
			"no_speech_prob", result.NoSpeechProb, "audio_duration_ms", duration.Milliseconds())
		ms.resolvePendingBargeIn()
		return
	}

	// Mid-thought pause guard: VAD's silence-based end-of-turn has no way to
	// know a brief pause (e.g. after a comma, mid-clause) isn't the user
	// actually finishing -- it just measures silence duration. A transcript
	// ending on a trailing comma/conjunction ("...and then,") is a cheap,
	// already-built-and-tested signal that this is exactly that case (see
	// turn_completion.go / TestIsLikelyComplete's "trailing comma = mid-
	// thought" case) -- but nothing in the pipeline actually consulted it
	// before this. Give a short, bounded window for a continuation before
	// committing the LLM to answering a half-sentence and the bot to
	// speaking right as the user keeps talking. Applied narrowly (only when
	// the text itself looks incomplete), not as a blanket wait on every
	// turn -- see the SILENCE_CONFIRMATION_MS removal above for why an
	// unconditional version of this was deliberately cut for latency.
	// Shadow-score Turno's turn-completion heads against the lexical gate.
	// This is the comparison that would justify promoting them: the lexical
	// verdict comes from a regex over the transcript after STT returns, the
	// Turno verdict from prosody during the speech itself. Neither gates
	// anything here -- but which branch we take below is a free ground-truth
	// label (user resumed = the turn really was incomplete), so every
	// incomplete-looking turn in production scores both predictors at once.
	ms.mu.Lock()
	ms.ckShadowMs = time.Since(ms.sttEndTime).Milliseconds()
	ms.mu.Unlock()

	lexicalComplete := ms.turnComp == nil || ms.turnComp.IsLikelyComplete(transcript)
	ms.mu.Lock()
	turnoLabel, turnoFrames := ms.turnoLastTurnLabel, ms.turnoTurnStateFrames
	turnoState, turnoHorizon := ms.turnoLastTurnState, ms.turnoLastHorizon
	ms.turnoTurnStateFrames = 0
	ms.mu.Unlock()
	if ms.turnoTurn != nil && turnoFrames > 0 {
		ms.logger.Info("Turno turn-completion shadow",
			"turno_model", "v6",
			"lexical_complete", lexicalComplete,
			"turno_label", turnoLabel,
			"p_complete", turnoState[0], "p_incomplete", turnoState[1],
			"p_backchannel", turnoState[2], "p_wait", turnoState[3],
			"p_end_200ms", turnoHorizon[0], "p_end_500ms", turnoHorizon[1],
			"p_end_800ms", turnoHorizon[2],
			"speech_frames", turnoFrames,
			"transcript", transcript)

		// Same observation, durably. The log line stays for live debugging;
		// this is what the analysis actually reads. No transcript goes in —
		// rows are aggregated across companies, so only derived features are
		// safe here.
		ms.recordExperiment(turnoTurnCompletionExperiment, "v6", ms.openingUnitID(seq),
			map[string]interface{}{
				"lexical_complete": lexicalComplete,
				"turno_label":      turnoLabel,
				"p_complete":       turnoState[0],
				"p_incomplete":     turnoState[1],
				"p_backchannel":    turnoState[2],
				"p_wait":           turnoState[3],
				"p_end_200ms":      turnoHorizon[0],
				"p_end_500ms":      turnoHorizon[1],
				"p_end_800ms":      turnoHorizon[2],
				"speech_frames":    turnoFrames,
				"transcript_chars": len(transcript),
				"transcript_words": countWords(transcript),
			}, nil)
	}

	// Turno hold: the transcript reads as finished, but prosody says the
	// speaker is not. Without this a sentence that merely *ends* — "I ordered
	// it last Tuesday." followed by a breath and "...and it never arrived" —
	// gets no confirmation window at all, because the lexical gate only fires
	// on text that looks incomplete. That is the case where the agent talks
	// over a continuation.
	turnoSaysHold := false
	if thr := ms.orch.config.TurnoHoldThreshold; thr > 0 && ms.turnoTurn != nil && turnoFrames > 0 {
		// p_incomplete + p_wait: "not finished" and "pausing, expecting to go
		// on" are both reasons to keep the floor with the caller.
		turnoSaysHold = (turnoState[1] + turnoState[3]) >= thr
	}

	if ms.turnComp != nil && (!lexicalComplete || turnoSaysHold) {
		waitMs := ms.orch.config.SilenceConfirmationMs
		if waitMs <= 0 {
			waitMs = 800
		}
		if lexicalComplete && turnoSaysHold {
			// Text looks done and only prosody disagrees, so this is a grace
			// window rather than a full mid-thought wait.
			hold := ms.orch.config.TurnoHoldMs
			if hold <= 0 {
				hold = 350
			}
			if hold < waitMs {
				waitMs = hold
			}
			ms.logger.Info("Turno hold: text looked complete but prosody says the speaker is not done",
				"p_incomplete", turnoState[1], "p_wait", turnoState[3],
				"threshold", ms.orch.config.TurnoHoldThreshold, "hold_ms", waitMs,
				"transcript", transcript)
		}

		// Turno horizon assist: when the head predicts the speaker was
		// finishing, shorten this wait instead of running it in full.
		//
		// Safe to drive from a low-precision signal specifically because of
		// where it sits. VAD has already reported end-of-turn by the time we
		// reach here, so a false positive cannot talk over anyone — the worst
		// it does is answer a half-finished sentence a little sooner, which is
		// the same risk the lexical gate beside it is already taking. The gate
		// still runs, and a user who resumes inside the shortened window still
		// reclaims the turn.
		horizonAssisted := false
		if thr := ms.orch.config.TurnoHorizonAssistThreshold; thr > 0 &&
			!lexicalComplete && !turnoSaysHold &&
			ms.turnoTurn != nil && turnoFrames > 0 && turnoHorizon[0] >= thr {
			factor := ms.orch.config.TurnoHorizonAssistFactor
			if factor <= 0 || factor >= 1 {
				factor = 0.25
			}
			floor := ms.orch.config.TurnoHorizonAssistMinMs
			if floor <= 0 {
				floor = 120
			}
			shortened := int(float64(waitMs) * factor)
			if shortened < floor {
				shortened = floor
			}
			if shortened < waitMs {
				ms.logger.Info("Turno horizon assist: shortening confirmation wait",
					"p_end_200ms", turnoHorizon[0], "threshold", thr,
					"wait_ms_before", waitMs, "wait_ms_after", shortened)
				waitMs = shortened
				horizonAssisted = true
			}
		}
		ms.mu.Lock()
		gate := make(chan struct{})
		ms.confirmationGate = gate
		ms.mu.Unlock()

		gateEnteredAt := time.Now()
		// Recorded immediately after the select below, NOT in a defer: defer
		// runs at function exit, which is after speakText has already called
		// logTurnLatency — so a deferred write is read as the reset zero every
		// time, which is exactly how this field came to exonerate the
		// confirmation gate without evidence.
		select {
		case <-gate:
			// User resumed speaking before the window elapsed: a mid-thought
			// pause, not a real turn end. Abandon this response -- the
			// continuation is already being captured as its own utterance by
			// onVADStart/handleAudio and will reach processUtterance on its
			// own once it, in turn, looks complete (or times out here too).
			// truly_incomplete=true is ground truth: the user did resume, so
			// the lexical gate was right to wait. The full Turno prediction
			// is repeated here rather than left to be joined against the
			// shadow line above -- log lines carry no call/utterance id, so
			// in a pod running concurrent calls a join would be guesswork.
			// Self-contained lines make the analysis a single pass.
			ms.logger.Info("Utterance looked incomplete and user resumed speaking, abandoning response",
				"transcript", transcript, "wait_ms", waitMs,
				"truly_incomplete", true, "horizon_assisted", horizonAssisted,
				"turno_model", "v6", "turno_label", turnoLabel,
				"p_complete", turnoState[0], "p_incomplete", turnoState[1],
				"p_backchannel", turnoState[2], "p_wait", turnoState[3],
				"p_end_200ms", turnoHorizon[0], "p_end_500ms", turnoHorizon[1],
				"p_end_800ms", turnoHorizon[2], "speech_frames", turnoFrames)
			ms.recordExperiment(turnoTurnCompletionExperiment, "v6", ms.openingUnitID(seq), nil,
				map[string]interface{}{"truly_incomplete": true, "wait_ms": waitMs,
					"horizon_assisted": horizonAssisted})
			ms.mu.Lock()
			if ms.confirmationGate == gate {
				ms.confirmationGate = nil
			}
			ms.mu.Unlock()
			ms.resolvePendingBargeIn()
			return
		case <-time.After(time.Duration(waitMs) * time.Millisecond):
			ms.mu.Lock()
			if ms.confirmationGate == gate {
				ms.confirmationGate = nil
			}
			ms.confirmWaitMs = time.Since(gateEnteredAt).Milliseconds()
			ms.mu.Unlock()
			// truly_incomplete=false: the lexical gate made us wait waitMs for
			// a continuation that never came. If Turno said "complete" here,
			// that wait was latency the horizon head could have saved. Note
			// this label means "did not resume within waitMs", not "the turn
			// was definitively over" -- a user resuming at waitMs+1 lands
			// here too, so it is a slightly optimistic negative.
			ms.logger.Info("Utterance looked incomplete but no continuation arrived, proceeding",
				"transcript", transcript, "wait_ms", waitMs,
				"truly_incomplete", false, "horizon_assisted", horizonAssisted,
				"turno_model", "v6", "turno_label", turnoLabel,
				"p_complete", turnoState[0], "p_incomplete", turnoState[1],
				"p_backchannel", turnoState[2], "p_wait", turnoState[3],
				"p_end_200ms", turnoHorizon[0], "p_end_500ms", turnoHorizon[1],
				"p_end_800ms", turnoHorizon[2], "speech_frames", turnoFrames)
			ms.recordExperiment(turnoTurnCompletionExperiment, "v6", ms.openingUnitID(seq), nil,
				map[string]interface{}{"truly_incomplete": false, "wait_ms": waitMs,
					"horizon_assisted": horizonAssisted})
		case <-ctx.Done():
			// The last silent exit in this function, and the expensive one. The
			// turn sits here waiting to see whether the caller is going to carry
			// on, and anything that cancels the pipeline during that window used
			// to return with no log, no event and no reply — the turn simply
			// evaporated. From the outside that is indistinguishable from the
			// service being down, and from the logs it is indistinguishable from
			// nothing having happened at all: a session read
			//
			//   processUtterance: entered > Utterance discarded > processUtterance: entered
			//
			// and then stopped, with a correct transcript already in hand.
			ms.logger.Warn("Utterance abandoned: cancelled while waiting to see if the caller continued — no reply for this turn",
				"seq", seq, "transcript", transcript, "wait_ms", waitMs,
				"ctx_err", ctx.Err())
			return
		}
	}

	// Barge-in confirmation gate: if this utterance tentatively interrupted a
	// still-speaking/processing bot, require at least MinWordsToInterrupt
	// words before committing to the interrupt — short interjections ("uh",
	// "yeah") that don't trip isLikelyNoise still shouldn't cut the bot off.
	// Below the threshold, resume instead of committing.
	//
	// Turno assist: if the bargein score peaked at/above
	// turnoBargeinAssistThr during this pending window, relax the word-count
	// requirement by turnoBargeinWordsRelief. This corroborates rather than
	// replaces the STT check — it can only ever lower the bar, never skip
	// it, and isLikelyEcho below still runs unmodified regardless.
	ms.mu.Lock()
	pendingBarge := ms.pendingBargeIn && ms.pendingBargeGen == ms.payloadGen
	turnoPeak := ms.turnoBargeinPeakScore
	ms.mu.Unlock()
	if pendingBarge {
		minWords := ms.orch.config.MinWordsToInterrupt
		effectiveMinWords := minWords
		turnoAssisted := ms.turno != nil && turnoPeak >= ms.turnoBargeinAssistThr
		if turnoAssisted {
			effectiveMinWords -= ms.turnoBargeinWordsRelief
			if effectiveMinWords < 0 {
				effectiveMinWords = 0
			}
		}
		if effectiveMinWords > 0 && countWords(transcript) < effectiveMinWords {
			// Keep the two-word guard for short noise/backchannels, but do
			// not make a sustained one-word command impossible to use. With
			// the current VAD hangover, a real "yes", "no", or "stop" turn
			// remains active long enough to qualify here; brief noise still
			// resolves as a false barge-in.
			if !acceptsSustainedSingleWordBargeIn(transcript, duration) {
				ms.logger.Info("Barge-in below MinWordsToInterrupt, resuming bot",
					"transcript", transcript, "word_count", countWords(transcript),
					"audio_duration_ms", duration.Milliseconds())
				ms.resolvePendingBargeIn()
				return
			}
		} else if turnoAssisted && countWords(transcript) < minWords {
			ms.logger.Info("Turno assist relaxed MinWordsToInterrupt",
				"transcript", transcript, "word_count", countWords(transcript),
				"turno_bargein_peak", turnoPeak)
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
				"transcript", transcript, "bot_was_saying", currentlySpeaking)
			ms.resolvePendingBargeIn()
			return
		}
	}
	ms.mu.Lock()
	ms.ckGateMs = time.Since(ms.sttEndTime).Milliseconds()
	ms.mu.Unlock()

	// Real, sufficient speech — commit to the interrupt now (cancels the old
	// pipeline, truncates spoken-truth context, emits Interrupted). No-op if
	// there was no pending barge-in for this generation.
	ms.confirmBargeInIfPending()

	ms.mu.Lock()
	ms.ckBargeMs = time.Since(ms.sttEndTime).Milliseconds()
	ms.mu.Unlock()

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
		// Mark the LLM stage as taken-but-instant, exactly as the speculative
		// path does. This branch returns before runLLMAndTTS, so it used to
		// leave llmStartTime/llmEndTime unset — the turn then logged llm_ms:-1,
		// llm_to_tts_ms:-1, and everything between STT and playback fell into
		// unaccounted_ms. That is what an "unexplained" 3-second turn looked
		// like. A cache hit is a real, measurable zero, not an absence.
		now := time.Now()
		ms.llmStartTime = now
		ms.llmEndTime = now
		ms.ttsStartTime = now
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
	ms.injectRagContext(ctx, transcript)

	ms.mu.Lock()
	ms.ckCacheMs = time.Since(ms.sttEndTime).Milliseconds()
	ms.mu.Unlock()

	if !ms.trySpeculativeResponse(ctx, transcript) {
		ms.runLLMAndTTS(ctx, transcript)
	}

	// Log latency breakdown for observability
	bd := ms.GetLatencyBreakdown()
	ms.logger.Info("utterance_latency",
		"stt_ms", bd.STT,
		"llm_ms", bd.LLM,
		"tts_first_ms", bd.LLMToTTSFirstByte,
		// ttfa_ms: true time-to-first-audio since the user actually
		// stopped speaking (userSpeechEnd -> first TTS byte) — this was
		// computed (UserToTTSFirstByte) but never logged; everything else
		// here is a narrower sub-interval of it (llm_ms is LLM-only,
		// tts_first_ms is LLM-end-to-TTS-first-byte, neither includes the
		// VAD hangover + STT time that comes first). This is the number
		// that answers "how fast does it respond after I stop talking".
		"ttfa_ms", bd.UserToTTSFirstByte,
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
	ms.pipelineCtx = rCtx
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()

	defer rCancel()

	ms.emitWithGen(BotThinking, nil, gen)
	// Work from here is discardable: if the caller resumes before playback
	// starts, everything after this point is thrown away.
	ms.mu.Lock()
	ms.discardStart = time.Now()
	ms.mu.Unlock()
	ms.llmStartTime = time.Now()
	// runStreamingLLM below only sets llmEndTime the first time it's zero
	// (capturing first-token arrival, not stream completion) — but that
	// guard never gets reset between turns, so from the second turn of a
	// call onward it kept the very first turn's value forever, making
	// every later llmEndTime - llmStartTime go strongly negative. Reset it
	// here so each turn's "first token" gets captured fresh.
	ms.llmEndTime = time.Time{}
	// Same reset, same reason, for ttsFirstChunkTime: it must capture only
	// the FIRST audio byte of this turn's response. speakText is called
	// once per sentence for multi-sentence responses, so without this
	// reset (and the matching fix removing speakText's own unconditional
	// overwrite — see there), a later sentence's start time silently
	// replaced the real first-byte timestamp, making ttfa_ms/tts_first_ms
	// measure "time to the LAST sentence" instead of the first — inflating
	// reported TTS latency by however long the earlier sentences took to
	// finish playing, on every multi-sentence reply.
	ms.ttsFirstChunkTime = time.Time{}
	// Reset with it: they are two ends of the same measurement and must come
	// from the same turn.
	ms.ttsStartTime = time.Time{}

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
	// responseChunksSent is truncateSpokenContext's signal for "did this
	// response actually play any audio before being interrupted" (see the
	// chunksSent==0 branch there). It was never reset per-turn anywhere in
	// this file — only ever incremented in speakText's chunk callback — so
	// once any earlier turn in the session had delivered at least one audio
	// chunk, the counter stayed >=1 for the rest of the call. A LATER turn
	// interrupted before its own TTS produced a single byte was then wrongly
	// treated as "something was spoken", and its never-spoken assistant
	// message was left in context instead of being removed. Reset here,
	// alongside the neighboring per-turn resets above, so the counter
	// reflects only the response about to be spoken.
	ms.responseChunksSent = 0
	ms.session.AddMessage("assistant", response)
	ms.emit(BotResponse, response)
	ms.cacheResponse(transcript, response, nil)

	// Full-response TTS (single pass, no sentence pipelining — avoids residual audio on interrupt)
	ms.speakText(rCtx, response, gen)
}

func (ms *ManagedStream) speakText(ctx context.Context, text string, gen int) {
	// A multi-sentence response is queued and spoken one sentence per
	// speakText call. If the turn's pipeline was already cancelled (e.g. the
	// user interrupted during the gap between two sentences — see
	// handleInterrupt's pipelineCtx check) by the time a later sentence's
	// turn comes up, don't speak it: without this, an interrupt landing
	// between sentences stopped emitting further audio for the CURRENT
	// sentence but still went on to speak every queued sentence after it,
	// since nothing here checked whether the pipeline had already been torn
	// down before starting a new one.
	if ctx.Err() != nil {
		// Say so. This return is correct but it was silent, and silent is what
		// made it untraceable: an utterance would be transcribed, answered, and
		// then produce no audio and no log line at all — the caller hears
		// nothing and the turn leaves no evidence it existed. Four turns of one
		// live call ended here or at the AudioChunk state gate with nothing
		// recorded anywhere.
		ms.logger.Info("Not speaking: pipeline already cancelled for this turn",
			"gen", gen, "text_len", len(text), "reason", ctx.Err().Error())
		return
	}

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

	// The late "did the user actually finish talking" recheck that onVADEnd
	// documents and depends on. Generation deliberately starts without a
	// pre-generation wait — that wait was most of the old per-turn latency —
	// and the design rests on catching a resumed speaker here instead. The
	// check was described in onVADEnd but never implemented, so a caller who
	// started talking again during generation got spoken over: no bot audio
	// existed yet, so nothing registered as a barge-in, no interrupt fired,
	// the pipeline context stayed live, and playback began on top of them.
	//
	// Abandon rather than queue. This response answers a turn the caller has
	// already moved on from, and the utterance they are speaking now will
	// produce its own.
	ms.mu.Lock()
	userTalking := ms.vadSpeaking
	ms.mu.Unlock()
	if userTalking {
		// Charge the abandoned work to discarded_ms rather than letting it swell
		// unaccounted_ms on whichever turn eventually plays. This is what made
		// one turn read 3966ms with 3002ms unexplained: not a stall, but a
		// response built for a turn the caller talked over, then rebuilt.
		ms.mu.Lock()
		if !ms.discardStart.IsZero() {
			ms.discardedMs += time.Since(ms.discardStart).Milliseconds()
			ms.discardStart = time.Time{}
		}
		if ms.state == StateProcessing {
			ms.state = StateListening
		}
		discarded := ms.discardedMs
		ms.mu.Unlock()
		ms.logger.Info("Discarding generated response: caller started speaking again before playback began",
			"text_len", len(text), "discarded_ms_total", discarded)
		return
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
	// Only the FIRST sentence of a turn sets ttsStartTime. speakText runs once
	// per sentence and used to overwrite it every time, while ttsFirstChunkTime
	// is captured once — so the two drifted apart within a turn and could even
	// end up from different turns, producing a tts_first_chunk_ms larger than
	// the whole e2e_ms it is supposedly part of (observed: 2029ms inside a
	// 1686ms turn, and a negative unaccounted_ms as a result). Reset per turn
	// alongside ttsFirstChunkTime, so this always means "start of synthesis for
	// this turn's first sentence".
	if ms.ttsStartTime.IsZero() {
		ms.ttsStartTime = ms.botSpeakStart
	}
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

	// Deliberately NOT setting ms.ttsFirstChunkTime here unconditionally —
	// speakText runs once per sentence for multi-sentence responses, and an
	// unconditional set here stomped whatever the first sentence's real
	// first-byte time was every time a later sentence started, silently
	// turning "time to first audio" into "time to the LAST sentence's
	// first audio". The IsZero()-guarded set inside the chunk callback
	// below is the only place this should be written; runLLMAndTTS resets
	// it to zero once per turn so that guard fires exactly once, on this
	// turn's true first byte.

	// Serialize TTS operations to prevent concurrent WS frame corruption.
	// Only one StreamSynthesize call per session at a time.
	ms.ttsMu.Lock()
	voice, lang := ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage()
	onChunk := func(chunk []byte) error {
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
			ms.logTurnLatency()
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
	}

	// If this exact segment was already rendered during the hangover, play it instead of
	// synthesising it again. Same bytes, same callback, same bookkeeping — the only difference is
	// that the work happened before the turn was confirmed rather than after, which is the entire
	// saving. See prerender.go.
	var err error
	if pre := ms.prerender.take(text, voice, lang); pre != nil {
		ms.logger.Info("Speaking pre-rendered opening segment", "chunks", len(pre), "chars", len(text))
		for _, c := range pre {
			if sCtx.Err() != nil {
				break
			}
			if e := onChunk(c); e != nil {
				err = e
				break
			}
		}
	} else {
		err = ms.orch.SynthesizeStream(sCtx, text, voice, lang, onChunk)
	}
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
	// Inactivity is measured from when the caller can actually respond, not
	// from stream creation or the start of a long bot response. Without this
	// refresh, a response longer than SilenceTimeout leaves lastActivityAt
	// stale; the monitor fires its "are you there" nudge immediately when
	// playback ends, which looks like the bot restarted/paraphrased itself.
	ms.lastActivityAt = time.Now()
	ms.mu.Unlock()
}

func (ms *ManagedStream) emitFrames(data []byte, frameSize, gen int) {
	if ms.turno != nil {
		farChunk16k := data
		if ms.playbackRate != 16000 {
			farChunk16k = resampleTo16k(data, ms.playbackRate)
		}
		ms.noteFarEndAudio(farChunk16k)
	}
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
	// Capture state BEFORE cancelling anything: cancelPipeline()'s cancel()
	// calls wake the in-flight TTS/pipeline goroutine, whose own cleanup
	// path (reacting to ctx.Done()) can win the race to grab ms.mu and reset
	// ms.state to something other than Speaking/Processing before this
	// function gets to read it below. That race meant "was this actually
	// interrupting something" sometimes read the POST-cancellation state
	// instead of the state at the moment the interrupt was requested — the
	// Interrupted event would silently never fire, which is what a real
	// barge-in on a call looks like getting dropped (bot doesn't stop, or
	// the next turn starts from stale state).
	ms.mu.Lock()
	oldState := ms.state
	// A multi-sentence response is synthesized one speakText() call per
	// sentence, and each call resets ms.state to StateIdle the moment its
	// own sentence finishes — so oldState can genuinely read Idle while the
	// turn as a whole is still very much in flight, waiting on the next
	// sentence. Checking pipelineCtx (un-Done for the turn's full duration,
	// not just one sentence of it) alongside oldState catches an interrupt
	// landing in exactly that gap, which oldState alone would silently miss
	// — the bot would keep talking through the rest of the response with no
	// Interrupted event ever firing.
	hadActiveTurn := ms.pipelineCtx != nil && ms.pipelineCtx.Err() == nil
	ms.state = StateInterrupted
	ms.interruptedAt = time.Now()
	// An interrupt is final. Anything held for a possible resume belongs to the
	// response being cut off, and must never surface on a later turn.
	ms.discardHeldAudioLocked()
	ms.mu.Unlock()

	ms.cancelPipeline()

	// Spoken-truth context: if the bot was interrupted mid-response, truncate
	// the last assistant message to only the text that was actually spoken.
	// This prevents the model from "remembering" things it never said.
	ms.truncateSpokenContext()

	if oldState == StateSpeaking || oldState == StateProcessing || hadActiveTurn {
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

// injectRagContext retrieves knowledge-base context for this turn and puts it
// in front of the model BEFORE the LLM call.
//
// It used to run in a detached goroutine "so it doesn't block the turn", which
// meant the retrieved context landed in the session at an arbitrary point —
// usually after the LLM call it was meant to inform had already been made. The
// turn it was retrieved for did not see it. Retrieval that arrives after the
// answer is not retrieval-augmented generation; it is a race the model usually
// loses.
//
// So this is synchronous, and bounded instead: the provider carries its own
// timeout (a few hundred milliseconds) and returns empty rather than erroring
// when nothing matches. The cost is real and it is on the critical path, which
// is the honest trade — it replaces a tool call that cost an entire extra LLM
// round trip plus a filler utterance to cover the gap.
func (ms *ManagedStream) injectRagContext(ctx context.Context, transcript string) {
	if ms.orch == nil || ms.orch.rag == nil {
		return
	}
	started := time.Now()
	contextText, err := ms.orch.rag.Retrieve(ctx, transcript)
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		// A knowledge base that is down must cost this turn its context, not
		// the turn itself. The model answers from its prompt.
		ms.logger.Warn("RAG retrieval failed, answering without knowledge context",
			"error", err, "elapsed_ms", elapsed)
		return
	}
	if contextText == "" {
		ms.logger.Info("RAG: nothing matched", "elapsed_ms", elapsed)
		return
	}
	// A system message, not a user one: this is reference material the model
	// may use, not something the caller said.
	ms.session.AddMessageRaw(Message{
		Role:    "system",
		Content: "[Knowledge base context for this question. Use it if relevant; do not mention that you looked it up.]\n" + contextText,
	})
	ms.logger.Info("RAG context injected",
		"query_len", len(transcript), "context_len", len(contextText), "elapsed_ms", elapsed)
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
	ms.pipelineCtx = nil
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
	var lang Language
	if ms.session != nil {
		lang = ms.session.GetCurrentLanguage()
	}
	if isRecogniserFiller(clean, lang, audioDuration) {
		return true
	}
	return false
}

// recogniserFillers are the short English phrases Parakeet emits when handed too
// little signal to work with. They are not transcription errors in the ordinary
// sense — nothing like them was said — they are what the model falls back to, and
// the same handful recurs across the industry ("thank you" above all).
//
// Compared case-insensitively with surrounding punctuation stripped, so entries
// here carry none of their own.
var recogniserFillers = map[string]bool{
	"thank you": true, "thanks": true, "you": true,
	"bye": true, "okay": true, "ok": true,
	"uh": true, "um": true, "hmm": true, "mm": true,
	"thanks for watching": true, "thank you for watching": true,
	"subtitles by the amara.org community": true,
}

// isRecogniserFiller reports whether a transcript is one of those fallbacks rather
// than something the caller said.
//
// Measured on production: 440ms of a Spanish caller's opening word came back as
// "Thank you.", which fired a complete turn — the agent answered "De nada.", began
// speaking it, and the caller's actual continuing sentence then registered as a
// barge-in on that reply. The caller heard nothing and had answered nothing. The
// existing guards all passed it: energy was high (RMS 0.21, well over
// PARAKEET_MIN_RMS), no_speech_prob was low, and at 10 characters it cleared the
// short-transcript check.
//
// An English filler phrase in a non-English call is essentially always this, so it
// is rejected outright. In an English call the phrase is genuinely sayable, so it
// needs the corroborating signal of implausibly short audio — nobody says "thanks
// for watching" in 400ms. The asymmetric cost justifies the asymmetric rule: a
// wrongly rejected "thanks" costs one missed turn the caller can simply repeat,
// while a wrongly accepted one spends a whole turn answering a phantom and then
// talks over the caller's real sentence.
func isRecogniserFiller(transcript string, lang Language, audioDuration time.Duration) bool {
	norm := strings.ToLower(strings.TrimSpace(transcript))
	norm = strings.TrimFunc(norm, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	if norm == "" || !recogniserFillers[norm] {
		return false
	}
	// An empty language is auto-detect, not "not English" — GetCurrentLanguage
	// returns "" for "auto"/"na". The caller may well be speaking English, so it
	// takes the same corroboration an English session does rather than the
	// outright rejection a known non-English session gets.
	if lang != LanguageEn && lang != "" {
		return true
	}
	return audioDuration < 700*time.Millisecond
}

func countWords(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Fields(s))
}

func acceptsSustainedSingleWordBargeIn(transcript string, duration time.Duration) bool {
	words := strings.Fields(transcript)
	if len(words) != 1 || duration < 600*time.Millisecond {
		return false
	}
	word := strings.TrimFunc(words[0], unicode.IsPunct)
	return len([]rune(word)) >= 2
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
	// STT on a bleed-through echo is often imperfect — phone-network
	// compression plus speaker-to-mic bleed plus a second STT pass produces
	// real insertions/deletions/substitutions, not just noise. On telephony
	// (no client-side AEC at all — see the caller's comment) this is the
	// only thing standing between the bot's own echo and a self-triggered
	// interrupt, so it needs real tolerance for garbled transcripts rather
	// than a near-exact match. 0.6 (was 0.8) accepts more of that noise;
	// worth erring toward "it's an echo" here since the failure mode of
	// wrongly resuming instead of interrupting is `wait`/`stop` occasionally
	// not landing, while the failure mode of missing a real echo is the
	// bot audibly restarting/repeating itself.
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
	return float64(matched)/float64(len(tWords)) >= 0.6
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
		if ms.session != nil && ms.session.GetCurrentVoice() != "" {
			voice = ms.session.GetCurrentVoice()
		} else if o != nil && o.config.VoiceStyle != "" {
			voice = o.config.VoiceStyle
		}

		clips := cachedBackchannelClips(ms.ctx, voice, func(c context.Context) [][]byte {
			out := make([][]byte, 0, len(backchannelPhrases))
			for _, phrase := range backchannelPhrases {
				// backchannelLang, not the caller's language — see backchannelPhrases.
				audio, err := o.GenerateSilent(c, phrase, voice, backchannelLang)
				if err == nil && len(audio) > 100 {
					out = append(out, audio)
				}
			}
			return out
		})

		if len(clips) > 0 {
			ms.backch.SetClips(clips)
		}
	}()
}

// splitSentences splits text into segments to synthesise, one per sentence, preserving the
// punctuation. Returns at least one segment.
//
// Only real sentence ends count: splitting at every '.' cut "a las 4 p.m." into three pieces and
// "3.14" into two, each becoming its own synthesis call — an audible gap between them, and too
// little text for the language token to condition, so a fragment came out English-sounding. A
// segment shorter than minSpokenSegment is joined to the next for the same reason. See
// sentence_boundary.go.
func splitSentences(text string) []string {
	var res []string
	rest := text
	for len(rest) > 0 {
		end := nextFlushPoint(rest, true, false, 0, minSpokenSegment)
		if end < 0 || end >= len(rest) {
			break
		}
		if s := strings.TrimSpace(rest[:end]); s != "" {
			res = append(res, s)
		}
		rest = strings.TrimLeft(rest[end:], " \t\n\r")
	}
	if s := strings.TrimSpace(rest); s != "" {
		res = append(res, s)
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

		// Clean up Turno models (gating instance + turn-completion shadow)
		if ms.turno != nil {
			ms.turno.Destroy()
		}
		if ms.turnoTurn != nil {
			ms.turnoTurn.Destroy()
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
	// Decide (and rate-limit) the dropped-audio warning while still holding the
	// lock — lastDropLogGen and state are both guarded by it, and the audio
	// path runs concurrently with the turn pipeline that mutates state.
	logDrop := false
	dropState := ms.state
	held := false
	if eventType == AudioChunk && !speaking {
		// A tentative barge-in is the one case where "not speaking" is provisional:
		// nothing has been confirmed, and the gate may reopen a moment from now. Hold
		// the frame so it can still be delivered if it does. Every other reason to be
		// off StateSpeaking (a confirmed interrupt, idle, a superseded generation) is
		// final, and those frames are dropped exactly as before.
		if chunk, ok := data.([]byte); ok && ms.pendingBargeIn && ms.pendingBargeGen == gen {
			if ms.heldAudioGen != gen {
				ms.heldAudio = nil
				ms.heldAudioBytes = 0
				ms.heldAudioGen = gen
			}
			// Past the cap, stop holding. A barge-in this long is almost certainly
			// real, and resuming several seconds late would be worse than not
			// resuming at all — the caller has moved on.
			if ms.heldAudioBytes+len(chunk) <= ms.maxHeldAudioBytesLocked() {
				c := make([]byte, len(chunk))
				copy(c, chunk)
				ms.heldAudio = append(ms.heldAudio, c)
				ms.heldAudioBytes += len(c)
				held = true
			}
		}
		if !held && ms.lastDropLogGen != gen {
			ms.lastDropLogGen = gen
			logDrop = true
		}
	}
	ms.mu.Unlock()

	if held {
		return
	}

	if eventType == AudioChunk && !speaking {
		// Every audio frame of a response can be dropped here — the stream is
		// not in StateSpeaking, so the caller gets silence — and this used to
		// happen without a single line anywhere. Logged once per generation
		// rather than per frame: the interesting fact is that a response was
		// silenced, not how many frames it had.
		if logDrop {
			ms.logger.Info("Dropping bot audio: stream is not in speaking state",
				"gen", gen, "state", dropState)
		}
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
		return
	default:
	}

	// The channel is full. This used to fall straight through to `default:` and
	// discard the event without a word, which for an AudioChunk means the caller
	// loses that slice of the reply — or, if it is the first chunk, the whole
	// reply — with nothing anywhere to say so. The backchannel sender two
	// functions down has always logged this case; the audio path never did, so
	// the one that matters was the one that was invisible.
	//
	// A dropped chunk is also unrecoverable in a way a dropped status is not, so
	// audio gets a short blocking retry first. 1024 events is ample for a reply
	// (~60 chunks); a full channel means the consumer is stalled, almost always
	// the websocket write applying backpressure from a slow reader. Waiting
	// briefly lets a transient stall drain instead of punching a hole in the
	// audio. The wait is deliberately short — past it the caller is better served
	// by the stream moving on than by a chunk arriving far too late.
	if eventType == AudioChunk {
		t := time.NewTimer(250 * time.Millisecond)
		defer t.Stop()
		select {
		case ms.events <- event:
			return
		case <-t.C:
		case <-ms.ctx.Done():
			return
		}
	}

	// Rate-limited to one line per generation: the interesting fact is that a
	// response lost audio, not how many chunks it lost.
	if ms.lastFullLogGen != gen {
		ms.lastFullLogGen = gen
		ms.logger.Warn("event channel full — DROPPING events",
			"type", eventType, "gen", gen, "cap", cap(ms.events),
			"note", "consumer stalled; an AudioChunk lost here is silence the caller hears")
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

// backchannelPhrases are the little sounds the agent makes while the caller is still talking.
//
// One list, no language switch, and that is the point. This used to be a 30-language table where
// each entry ended in a real word — "sí", "oui", "ja", "sim", "bai", "yeah" — and the trouble with
// a word is that it is only right in one language. Any gap in the table fell through to the
// English default, so a Catalan conversation got "yeah"; any language served by a near-miss got a
// word a native speaker would notice. Adding a language meant remembering to add a row, and
// forgetting was silent.
//
// These are not words in any language. A nasal hum and an open vowel are what listeners produce to
// say "go on, I'm still here" across essentially every language, and none of them is lexical, so
// none can be wrong in one. That removes the failure mode rather than widening the table.
//
// Deliberately excluded: "uh-huh" and "yeah" (English interjections, unmistakable in a Spanish
// call) and "aha" (reads as recognition — "I see" — rather than mere attention, and in several
// languages it lands as surprise).
var backchannelPhrases = []string{"mhm", "mm", "hm"}

// backchannelLang is the language these are synthesised in — English, which the model treats as its
// unmarked base case and therefore prefixes with no language token.
//
// Passing the caller's language would prepend [es] or [ca] to a sound that is not a word in
// Spanish or Catalan, which asks the text encoder to apply a language's pronunciation prior to
// something that has no pronunciation in it. Unmarked is the honest description of a nasal hum.
const backchannelLang = LanguageEn

func backchannelPhrasesForLang(Language) []string { return backchannelPhrases }

func (ms *ManagedStream) generateBackchannelClips(o *Orchestrator) {
	voice := VoiceF1
	if ms.session != nil && ms.session.GetCurrentVoice() != "" {
		voice = ms.session.GetCurrentVoice()
	} else if o != nil && o.config.VoiceStyle != "" {
		voice = o.config.VoiceStyle
	}

	// Once per process per (voice, language) — not once per session. See backchannel_cache.go:
	// regenerating identical audio at every session start took a synthesis slot from the caller's
	// first sentence and pushed it over real-time, which is heard as a gap.
	clips := cachedBackchannelClips(ms.ctx, voice, func(c context.Context) [][]byte {
		out := make([][]byte, 0, len(backchannelPhrases))
		for _, phrase := range backchannelPhrases {
			// backchannelLang, not the caller's language — see backchannelPhrases.
			audio, err := o.GenerateSilent(c, phrase, voice, backchannelLang)
			if err == nil && len(audio) > 100 {
				out = append(out, audio)
			}
		}
		return out
	})

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
						// StateListening is recoverable too. The outer check
						// already required that VAD reports no speech and that
						// nothing has happened for the whole timeout, so a
						// stream still sitting in Listening under those
						// conditions is not waiting for anyone — it is stuck,
						// and the caller is hearing silence. This was the gap
						// that let a dropped or discarded utterance strand a
						// call indefinitely.
						recoverable := ms.state == StateIdle ||
							ms.state == StateInterrupted ||
							ms.state == StateListening
						// At most one nudge per idle stretch — cleared by
						// onVADStart the next time the user actually speaks.
						// Without this, every subsequent 2s tick still finds
						// lastActivity stale (nothing here ever refreshes it
						// once the user has gone silent) and fires again,
						// producing an unbounded loop of freshly-reworded
						// "are you there" nudges instead of one prompt
						// followed by real silence.
						if !recoverable || ms.vadSpeaking || ms.silenceNudgeSent {
							ms.mu.Unlock()
							return
						}
						ms.silenceNudgeSent = true
						ms.mu.Unlock()
						ms.runLLMAndTTS(ms.ctx, "[USER_SILENCE_TIMEOUT]")
					}()
				}
			}
		}
	}
}

// speakResponse says a whole LLM reply, one synthesis call per segment.
//
// The streaming path already does this: it flushes to TTS at sentence boundaries as tokens arrive,
// so no single call ever gets the whole reply. The paths that receive a COMPLETE response — a
// speculative hit, a non-streaming LLM, a cached answer — handed the lot to speakText in one call,
// and that is a different thing entirely.
//
// It matters because synthesis cost is superlinear in text length (no KV cache in the velocity
// field, so every block re-runs over the whole prefix). One long call climbs past real time and the
// caller hears the reply stop and restart — the gaps reported from live calls. The cap that
// prevents it lives in nextFlushPoint, which the streaming path goes through and these did not. So
// the common case, a speculative hit, was the one case still exposed to it.
//
// Splitting also arrives sooner: the first segment is shorter than the whole reply, so the first
// block of audio is ready earlier. Prosody is unaffected because the cut is at a sentence end,
// which is where a speaker pauses anyway — unlike cutting the opening chunk mid-sentence, which
// was tried, sounded like the agent stopping dead after five words, and is deliberately off.
func (ms *ManagedStream) speakResponse(ctx context.Context, text string, gen int) {
	for _, seg := range splitSentences(text) {
		if ctx.Err() != nil {
			return // interrupted between sentences; speakText would refuse anyway
		}
		ms.speakText(ctx, seg, gen)
	}
}
