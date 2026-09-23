package orchestrator

import (
	"context"
	"strings"
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
	// BotResumed corrects a status the client was already told ("listening",
	// from the tentative barge-in's own onVADStart) once that barge-in
	// resolves as a false alarm. Data is a string: "speaking" or "thinking" --
	// whichever status the client should show now.
	BotResumed EventType = "BOT_RESUMED"
	// BotResponseTruncated: the caller cut the agent off. Data is a TruncatedResponse — what they
	// actually heard. A reply is committed (and sent as a BotResponse) once it is synthesized, which
	// is long before it has played, so without this every transcript records the whole reply.
	BotResponseTruncated EventType = "BOT_RESPONSE_TRUNCATED"
	// TranscriptRevised: the caller's previous turn and what they said next were one utterance,
	// transcribed together. Data is a RevisedTranscript. Emitted instead of a TranscriptFinal.
	TranscriptRevised EventType = "TRANSCRIPT_REVISED"
	// StopPlayback: for a transport that plays through tentative barge-ins
	// (SetTransportPlaysThroughBargeIn), the caller has kept talking over the agent long enough —
	// stop playback now and discard what is queued. Interrupted may still follow if the barge-in is
	// confirmed; transports that pause or stop at the onset can ignore it.
	StopPlayback EventType = "STOP_PLAYBACK"
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
	// Catalan, Galician and Basque: three of the nine languages Lokutor actually ships (see
	// pkg/api/languages.go), and the three that were missing here. Their absence was not inert —
	// languageCodeToName falls back to the raw code, so a Catalan agent instructed the model with
	// "Always respond in ca", which is not a language name and reads as noise next to a transcript
	// that looks Spanish. The model did the reasonable thing with an unreadable instruction and
	// answered in Spanish.
	LanguageCa Language = "ca"
	LanguageGl Language = "gl"
	LanguageEu Language = "eu"
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

// OpeningTrigger is the minimal nudge that makes the model take the first
// turn. It deliberately prescribes NO content.
//
// The orchestrator has no business deciding what an agent opens with. It used
// to inject "give a brief greeting and ask how you can help", which — because
// it goes in as a USER-role message, outranking the agent's system prompt for
// that turn — made every agent behave like an inbound receptionist. An
// outbound sales agent configured to pitch opened by asking the person it had
// just cold-called how it could help them.
//
// Anything about WHAT to say belongs in the agent's own prompt (or in
// OpeningMessage). This constant only says "your turn".
const OpeningTrigger = "The conversation has just started and you are speaking first. Open it now, in the configured language, following your own instructions."

// resolveOpening decides how the bot's first turn is produced. Exactly one of
// the two results is non-empty: a verbatim message to speak (no LLM call), or
// the instruction to hand the LLM.
//
// Extracted so it can be tested directly. The "outbound agent opens by asking
// how it can help" bug shipped twice because the real decision lived inline in
// a goroutine where nothing could assert on it.
func resolveOpening(cfg Config) (verbatim string, instruction string) {
	// A recording notice is a legal disclosure, so it is never left to the
	// model: it is spoken verbatim, before anything else, whatever the agent's
	// prompt says. Folding it into a verbatim opening keeps it to one TTS turn;
	// the LLM-opening path speaks it separately (see managed_stream).
	notice := strings.TrimSpace(cfg.RecordingNotice)
	if msg := strings.TrimSpace(cfg.OpeningMessage); msg != "" {
		if notice != "" {
			return notice + " " + msg, ""
		}
		return msg, ""
	}
	instr := strings.TrimSpace(cfg.OpeningInstruction)
	if instr == "" {
		instr = OpeningTrigger
	}
	return "", instr
}

type FirstSpeaker string

const (
	FirstSpeakerUser FirstSpeaker = "user"
	FirstSpeakerBot  FirstSpeaker = "bot"
)

type Config struct {
	SampleRate         int
	Channels           int
	BytesPerSamp       int
	MaxContextMessages int
	// MaxContextTokens caps conversation context by size. Zero takes DefaultMaxContextTokens; set
	// it negative to disable the cap entirely. See context_budget.go for the measured reason this
	// exists — prompt size is both ~84ms of first-token latency per 1,000 tokens and about 76% of
	// variable cost per call-minute.
	MaxContextTokens         int
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

	// TurnoEarlyEndThreshold, when > 0, lets Turno's horizon head cut the VAD
	// hangover short. The hangover (~448ms) exists to be certain a pause is
	// really the end of a turn using energy alone; a turn-detection model
	// removes exactly that uncertainty, so sitting out the full wait when the
	// model is confident is latency spent for nothing.
	//
	// Off by default. It is the one signal here that can make the agent
	// interrupt: everything downstream (the lexical gate, the confirmation
	// window) still runs, so a false positive costs a slightly early answer
	// rather than talking over someone — but it is still the riskiest of the
	// Turno hooks, so it ships dark and is enabled deliberately.
	TurnoEarlyEndThreshold float32

	// TurnoEarlyEndMs is the silence required once the horizon head is
	// confident. Floored at TurnoEarlyEndMinMs so this can never become a
	// hair-trigger.
	TurnoEarlyEndMs int

	// RecordingNotice, when non-empty, is spoken verbatim at the very start of
	// the call, before any substantive conversation. It exists to satisfy
	// call-recording consent law, which is why it must not be left to the
	// model to remember: an agent that forgets to say it turns a recorded call
	// into an unlawful one.
	RecordingNotice string

	// OpeningMessage, when non-empty, is spoken verbatim as the bot's first
	// turn and no LLM call is made at all. This is opt-in: a customer who
	// wants a scripted first line gets exactly that line every time, and the
	// call skips an LLM round-trip. Leave it empty and the agent improvises
	// its own opening from its prompt, which is the default.
	//
	// Takes precedence over OpeningInstruction.
	OpeningMessage string

	// TurnoHoldThreshold guards the opposite failure from the horizon assist:
	// the speaker finishes a sentence, pauses, and carries on — but the
	// transcript already reads as complete, so the lexical gate applies no
	// confirmation wait at all and the agent talks over the continuation.
	//
	// Turno's TurnState head sees prosody rather than punctuation, so it can
	// tell "finished a sentence" from "finished speaking". When
	// p_incomplete + p_wait reaches this value the turn is held briefly even
	// though the text looks complete.
	//
	// A false positive here costs a short delay; a false negative talks over
	// the user. Those are not symmetric, which is why this errs toward
	// holding. Zero disables the hold.
	TurnoHoldThreshold float32

	// TurnoHoldMs is how long a Turno-flagged turn is held. Deliberately much
	// shorter than SilenceConfirmationMs: the text does look complete, so this
	// is a grace window, not a full mid-thought wait.
	TurnoHoldMs int

	// TurnoHorizonAssistThreshold: when Turno's horizon head predicts the
	// speaker is finishing (p_end_within_200ms at or above this value), the
	// mid-thought confirmation wait is shortened by
	// TurnoHorizonAssistFactor rather than run in full.
	//
	// This is deliberately placed AFTER VAD end-of-turn, which is what makes
	// a low-precision signal safe to use here: by this point the user has
	// already stopped speaking, so a false positive cannot cut anyone off —
	// it can only make the agent answer a half-finished sentence slightly
	// sooner, which is the exact risk the lexical gate beside it already
	// manages. Used to shorten, never to skip: the gate still runs and the
	// user can still reclaim the turn.
	//
	// Default 0.35, not 0.5: the v6 horizon head's scores peak around 0.48 on
	// real speech, so a conventional cutoff would never fire at all.
	// Zero disables the assist.
	TurnoHorizonAssistThreshold float32

	// TurnoHorizonAssistFactor scales the confirmation wait when the horizon
	// head corroborates. Floored by TurnoHorizonAssistMinMs so the gate keeps
	// a real window for the user to resume.
	TurnoHorizonAssistFactor float64

	// TurnoHorizonAssistMinMs is the floor on a shortened wait.
	TurnoHorizonAssistMinMs int

	// RecordExperiment, when set, receives champion/challenger observations
	// (see the turn-completion shadow in turno_bargein.go). A hook rather than
	// a direct dependency: this module has no business knowing where
	// observations are stored, and the host already owns that.
	//
	// Implementations MUST NOT block — this is called from the audio path.
	// Nil means no recording.
	RecordExperiment func(experiment, variant, unitID string, metrics, label map[string]interface{})

	// OpeningInstruction overrides the nudge that triggers the bot's first
	// turn. Injected as a USER-role message, so it outranks the agent's system
	// prompt for that turn — which is exactly why the default (OpeningTrigger)
	// prescribes no content at all. Set this only to change the *mechanism*,
	// never to put words in an agent's mouth; that belongs in its prompt.
	//
	// Empty means OpeningTrigger.
	OpeningInstruction string

	// PostInterruptBackoff: after a confirmed barge-in, wait this long from
	// the interrupt (not from when the response is ready) before the bot's
	// next reply starts speaking — avoids immediately talking back over a
	// user who paused mid-thought (Vapi backoffSeconds pattern). By the time
	// a reply is ready to speak, STT+LLM processing has usually already
	// eaten a few hundred ms of this window, so it rarely adds its full
	// value on top — but keep it short: it's dead air on every single
	// barge-in, not just edge cases.
	PostInterruptBackoff time.Duration

	// SilenceConfirmationMs: after VAD detects speech end, wait this many ms
	// for the user to resume speaking before triggering the LLM pipeline.
	// Prevents phantom interruptions from brief pauses between sentences.
	SilenceConfirmationMs int

	// Client-side VAD: server accepts vad_speech_start/end control frames
	ClientVAD bool

	// Token-level TTS: send text to TTS on smaller boundaries (N words or after comma)
	TokenLevelTTS bool

	// Number of words between TTS flushes when TokenLevelTTS is enabled (0 = disabled)
	TTSMinTokenInterval int

	// Speculative LLM: start LLM during speech based on partial audio
	SpeculativeLLM bool

	// SpeculativePrerender: also render the opening segment's AUDIO during the VAD hangover, not
	// just the reply text. Speculation already takes stt_ms and llm_ms to zero on a hit; this takes
	// tts_first_chunk_ms too, which measurement showed was the entire remaining budget besides the
	// hangover itself (e2e 353ms = hangover 245 + tts_first 107). Requires SpeculativeLLM.
	//
	// OFF BY DEFAULT, and the reason is the whole trade. It needs a SPARE synthesiser slot. The
	// production node carries exactly one concurrent stream, so the pre-render and the real
	// synthesis are mutually exclusive: turned on there, the render held the only slot for ~1s,
	// the confirmed path got "503 busy", and turns went from 353ms to 14.4 SECONDS. The feature
	// is sound and the node is too small for it.
	//
	// Enable with SPECULATIVE_PRERENDER=1 only where the synthesiser has more than one stream —
	// a c7a voice node carries four, so one spent on a guess still leaves three. Check the
	// sidecar's calibration line ("now serving N concurrent stream(s)") before turning it on.
	SpeculativePrerender bool

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

	// Turno: ONNX model path for the dual-channel (near+far) model. Runs
	// alongside VAD, continuously, for two purposes (see
	// turno_bargein.go): a log-only VAD shadow comparison against ms.vad,
	// and a barge-in assist that relaxes (never bypasses)
	// MinWordsToInterrupt. It does not replace turn-state detection (its own
	// benchmark shows that head loses to a naive baseline). On by default
	// (see DefaultConfig); set to "" to disable.
	TurnoModelPath string

	// TurnoTurnModelPath: a SECOND Turno instance, loaded only to record the
	// TurnState and Horizon heads for the turn-completion shadow (see
	// managed_stream.go's mid-thought guard). Its VAD and bargein outputs are
	// discarded — TurnoModelPath above remains the only model that gates
	// anything.
	//
	// This exists because the gating model (v3_aec) has a dead horizon head:
	// 0.000/0.000/0.060 recall at 200/500/800ms, and measured on real speech
	// its p_end_200ms never exceeds 0.062, so it cannot cross any usable
	// threshold. Shadow-logging horizon off it would yield a confident-looking
	// dataset that means nothing. checkpoints_v6 has the fix and is
	// signature-identical, so it loads into the same runtime unchanged.
	//
	// Set to "" to disable the turn-completion shadow. Failure to load is
	// non-fatal and leaves every gating path untouched.
	TurnoTurnModelPath string

	// TurnoBargeinAssistThreshold: while a tentative barge-in is open, if
	// Turno's bargein score peaks at or above this value, processUtterance
	// relaxes MinWordsToInterrupt by TurnoBargeinAssistWordsRelief for
	// that utterance instead of requiring the full word count. This
	// corroborates the STT gate; it cannot skip it.
	//
	// History: this field used to be a fast-confirm bypass threshold (a
	// score at/above it committed the interrupt immediately, no STT
	// involved) — tuned against real production traffic from 0.85 -> 0.6 ->
	// 0.8 after the original model (trained only on OpenYAP/oto, with no
	// acoustic echo path between channels) proved to have little real
	// signal on this pipeline's actual audio: 0.6 confirmed a real "mhh"
	// backchannel as a genuine interrupt on a real test call. The bypass was
	// retired entirely (not just re-tuned) after root-causing that failure
	// to a real gap — zero training exposure to device echo — and retraining
	// against Microsoft's AEC Challenge real-device recordings
	// (turn-taking/checkpoints_v3_aec). That retrain cut the false-confirm
	// rate on held-out real echo from 93.0% to 11.4%, but true-confirm speed
	// on real interruptions also dropped (median 140ms -> 3.46s, recall
	// 100% -> 84.8%) and a threshold/debounce sweep confirmed there's no
	// operating point that recovers both — a hard tradeoff in the model's
	// score distribution, not a tuning gap. A "fast" path slower than just
	// waiting for STT isn't a bypass worth having, so Turno's bargein
	// score is now only ever an assist on top of STT, which doesn't need to
	// be fast. 0.8 carries over as a starting point from the old bypass
	// tuning — re-tune against the "Turno frame" bargein logs from real
	// calls with the new model, the same way the old threshold was tuned.
	//
	// Turno originally also had a symmetric ResolveThreshold: a low
	// score would resolvePendingBargeIn() (declare "just a backchannel,
	// keep talking"). Removed entirely after production showed genuine
	// interruption attempts commonly scoring in the same ~0.37-0.5 band as
	// backchannels on this pipeline's real audio — the resolve path was
	// actively vetoing real barge-ins, making the bot impossible to
	// interrupt, which is strictly worse than a wrong assist (that just
	// costs one utterance needing one fewer word than usual). The decision
	// to stand down stays entirely with the existing STT-based checks
	// (MinWordsToInterrupt/isLikelyNoise/isLikelyEcho), which worked
	// correctly on their own before Turno existed.
	TurnoBargeinAssistThreshold float32

	// TurnoBargeinAssistWordsRelief: how many fewer words
	// MinWordsToInterrupt requires for an utterance where
	// TurnoBargeinAssistThreshold was met. Floored at 0 words (never
	// fewer) — this narrows the gate, it does not remove it.
	TurnoBargeinAssistWordsRelief int

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
		MaxContextTokens:         DefaultMaxContextTokens,
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
		// reply — reduced to 100ms since echo suppressor now handles false
		// interrupts at the Telnyx inbound layer.
		PostInterruptBackoff: 100 * time.Millisecond,

		// SilenceConfirmationMs: after VAD detects speech end, wait this long
		// for the user to resume speaking before triggering the LLM pipeline.
		// Prevents "phantom interrupts" where a brief pause (exceeding the
		// VAD silence limit) triggers the bot while the user continues speaking.
		SilenceConfirmationMs: 800,

		ClientVAD:             false,
		TokenLevelTTS:         true,
		TTSMinTokenInterval:   4,
		SpeculativeLLM:        true,
		SpeculativePrerender:  speculativePrerenderEnabled(),
		SpeculativeIntervalMs: 300,
		AdaptivePacing:        true,
		ResponseCaching:       true,
		TTSConnectionPoolSize: 3,
		ContextSummarization:  true,
		SummarizationPrompt:   "Summarize the following conversation turns in 1-2 sentences, keeping key facts and context:",
		STTRegion:             "",
		LLMRegion:             "",
		TTSRegion:             "",

		// On by default — see TurnoModelPath's doc comment. Set to "" to
		// disable (e.g. if the model asset genuinely isn't present).
		TurnoModelPath:     "assets/onnx/turno/model.onnx",
		TurnoTurnModelPath: "assets/onnx/turno/turn_v6.onnx",
		// 0 = off. Was 0.45, which fired on essentially every turn: the
		// hold triggers on p_incomplete + p_wait, and the v6 head reports
		// p_incomplete of 0.67-0.72 even on plainly finished sentences, so
		// the threshold was met constantly. Measured in production on
		// 2026-09-15: all three turns of a live call logged "Turno hold" and
		// paid the full 350ms, on transcripts like "Hello, how are you?" —
		// a flat 350ms added to every turn by a head that is miscalibrated
		// in one direction. The shadow logging beside it stays on, and this
		// goes back above zero when it says the head has been fixed.
		TurnoHoldThreshold:            0,
		TurnoHoldMs:                   350,
		TurnoHorizonAssistThreshold:   0.35,
		TurnoHorizonAssistFactor:      0.25,
		TurnoHorizonAssistMinMs:       120,
		TurnoBargeinAssistThreshold:   0.8,
		TurnoBargeinAssistWordsRelief: 1,
		VoiceUXInstructions:           "",
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

	// MaxContextTokens caps the conversation by SIZE, which is what actually costs anything.
	// MaxMessages caps it by count, and count is a poor proxy: one long turn can carry more than
	// twenty short ones. Zero disables the token cap and leaves the count cap alone.
	MaxContextTokens int

	// basePrompt is the agent's own prompt, before buildSystemPrompt wraps it in the guidelines
	// and the language section. Kept so that a language change can REBUILD the system prompt
	// rather than patch it.
	//
	// Patching is what it used to do, and it produced a prompt that argued with itself. The
	// language section names the language nine times; the patch was a regex for the one sentence
	// "Always respond in X." So the usual startup order — SetSystemPrompt (session still on its
	// LanguageEn default) then SetLanguage(agent's language) — left one line saying Spanish and
	// eight still saying English, including the emphatic ones: "still reply in English", "every
	// word you produce must be in English", "answer it in English". Faced with 8-to-1 the model
	// drifted to English mid-call, on Spanish calls, which is exactly what callers reported.
	basePrompt string
}

func NewConversationSession(userID string) *ConversationSession {
	return &ConversationSession{
		ID:               userID,
		Context:          []Message{},
		MaxMessages:      20,
		MaxContextTokens: DefaultMaxContextTokens,
		CurrentVoice:     VoiceF1,
		CurrentLanguage:  LanguageEn,
		toolCallCounts:   make(map[string]int),
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
		// Keep the leading system message. The trim used to be a plain tail slice, which drops
		// index 0 — and index 0 is the system prompt, carrying the entire language section, the
		// guardrails and the agent's own instructions. A call long enough to reach MaxMessages
		// therefore lost every rule holding it to one language, silently, in the middle of a
		// conversation. Nothing logged it and the model simply started behaving like a different
		// agent.
		//
		// This is not hypothetical head-room: context on a real call was measured growing past
		// 7,000 tokens with no ceiling in sight, because summarizeContextIfNeeded — the function
		// meant to cap it — is never called from anywhere.
		if len(s.Context) > 0 && s.Context[0].Role == "system" {
			keep := s.MaxMessages - 1
			if keep < 1 {
				keep = 1
			}
			tail := s.Context[1:]
			if len(tail) > keep {
				tail = tail[len(tail)-keep:]
			}
			s.Context = append(s.Context[:1:1], tail...)
		} else {
			s.Context = s.Context[len(s.Context)-s.MaxMessages:]
		}
	}
	s.trimToTokenBudgetLocked()
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
