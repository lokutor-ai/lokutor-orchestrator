package orchestrator

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backchannels (2026-11-19): while the caller talks, the agent makes the small listener sounds a person makes in
// the pauses ("mhm", "ajá", "ah", a soft "oh...", a rising "oh!"), in its own voice, about a quarter of a second
// into a pause, as a person does.
//
// WHEN. On the caller's own pause, from the VAD's frames, not from a transcript: once a pause after a speech run
// reaches backchannelPauseDelay (the VAD is usually still in its hangover then). Only after
// backchannelMinSpeech of caller speech since the agent last spoke or the last backchannel (so never in the
// first seconds of a turn), at most every backchannelMinInterval, never while the agent's audio plays or a
// barge-in is pending. The caller may in fact have finished: "mhm" and then the reply is how people talk, and the
// reply's audio is queued after the sound (its playout starts where the sound ends), never over it. End of turn
// is not touched: the VAD and every hold run as they would without the sound.
//
// WHICH SOUND. A pick kept per stream, refreshed in the background whenever new caller words exist (each pause's
// speculative transcript, each utterance's final): one small request to the agent's own model that answers a
// category: neutral (a continuer), ack, sympathy, surprise or none (the caller is dictating, spelling, or has just
// asked the agent something). At the pause the current pick plays at once; with no pick yet, neutral. The pick
// is never waited for, so it can be one pause old.
//
// WHAT IT SOUNDS LIKE. Never an isolated word synthesised at the pause: the call makes its own sounds at its start
// (backchannel_sounds.go), in its voice and language, cut out of short carrier phrases; until they exist the call
// simply has no backchannels. A host may supply them instead (BackchannelBankProvider).
//
// ECHO. The sound is registered as the agent's own audio on the echo check's far end and Turno's. For as long as
// it plays plus backchannelEchoTail the VAD and the speech buffers get silence in place of the microphone
// (the real audio is kept). When the window closes: if the caller is talking (the last 100 ms well above the
// pause's own level), the kept audio is replayed into the normal path, so nothing they said is lost; otherwise it
// was the sound's echo and is dropped. A caller who resumes over the sound is heard up to one window late.
//
// NOT A TURN. Nothing is written to the conversation, the stream's state does not change.

const (
	backchannelPauseDelay  = 250 * time.Millisecond
	backchannelLatePause   = 600 * time.Millisecond // past this the pause is no longer "just now": no sound
	backchannelMinSpeech   = 2200 * time.Millisecond
	backchannelMinInterval = 4500 * time.Millisecond
	backchannelEchoTail    = 300 * time.Millisecond
	backchannelPickTimeout = 2 * time.Second
	backchannelPickWords   = 3                       // a new pick once the caller's words have grown by this many
	backchannelPickGap     = 1500 * time.Millisecond // and at most one pick request this often
	backchannelMaxTokens   = 4
	backchannelMaxHeld     = 3 * time.Second
	backchannelHeardReset  = time.Second      // agent audio heard this long starts the caller's turn over
	backchannelBankRetry   = 30 * time.Second // a voice whose bank was not there yet (being built) is asked again after this
	backchannelBankTimeout = 3 * time.Minute  // a first build on the synthesis pool takes seconds; the request may wait for it
)

// Backchannel categories: the bank's and the pick's.
const (
	BackchannelNeutral  = "neutral"
	BackchannelAck      = "ack"
	BackchannelSympathy = "sympathy"
	BackchannelSurprise = "surprise"
	BackchannelNone     = "none"
)

// BackchannelBank is one voice's listener sounds in one language: PCM16 mono at SampleRate, by category.
type BackchannelBank struct {
	SampleRate int
	Clips      map[string][][]byte
}

// BackchannelBankProvider gives a voice's bank, or nil when the voice has none (yet). The host's TTS provider
// implements it.
type BackchannelBankProvider interface {
	BackchannelBank(ctx context.Context, voice Voice, lang Language) (*BackchannelBank, error)
}

const backchannelPickPrompt = `You decide which listener sound fits while someone talks on a phone call, the small sound a person makes to show they follow. Answer with ONE word:
neutral - they are telling or explaining something (a plain "mhm")
ack - they confirmed or clarified something ("ah", "ah, sí")
sympathy - something bad or sad happened to them (a soft "oh...")
surprise - something unexpected or remarkable ("oh!")
none - ONLY while they dictate or spell (numbers, codes, an address, a name letter by letter) or right after they asked the agent a direct question
When unsure, neutral.
Answer only the word.`

// BackchannelPickMessages is the request for the category, from the agent's last words and the caller's so far.
func BackchannelPickMessages(lang Language, agentLine, callerText string) []Message {
	var b strings.Builder
	if lang != "" {
		b.WriteString("Language: " + string(lang) + "\n")
	}
	if a := strings.TrimSpace(agentLine); a != "" {
		b.WriteString("Agent's last words: \"" + a + "\"\n")
	}
	b.WriteString("Caller so far (still talking): \"" + strings.TrimSpace(callerText) + "\"\nSound:")
	return []Message{{Role: "system", Content: backchannelPickPrompt}, {Role: "user", Content: b.String()}}
}

// BackchannelContext files the request's tokens under TokensBackchannel and caps its answer.
func BackchannelContext(ctx context.Context) context.Context {
	return WithMaxTokens(WithTokenCategory(ctx, TokensBackchannel), backchannelMaxTokens)
}

// ParseBackchannelPick is the category in the model's answer, or "" when it named none of them.
func ParseBackchannelPick(s string) string {
	w := strings.ToLower(strings.Trim(strings.TrimSpace(s), " .\"'*\n"))
	if i := strings.IndexAny(w, " \n,"); i > 0 {
		w = w[:i]
	}
	switch w {
	case BackchannelNeutral, BackchannelAck, BackchannelSympathy, BackchannelSurprise, BackchannelNone:
		return w
	}
	return ""
}

// backchannelState is a stream's backchannel bookkeeping (its own lock; never held with ms.mu taken after it).
type backchannelState struct {
	mu sync.Mutex

	bank      *BackchannelBank // at the playback rate, for bankKey
	bankKey   string
	loading   bool
	bankRetry time.Time // a load that found no bank is tried again from here
	lastClip  []byte

	// the pick
	pick       string
	pickText   string // the caller words it was made from
	pickAt     time.Time
	inflight   bool
	pickAsked  time.Time
	latestText string // the newest caller words seen
	turnText   string // the caller's finals since the agent last spoke

	// the trigger
	speech      time.Duration // caller speech since the agent was last heard or the last backchannel
	agentFrom   time.Time     // the agent's audio has been playing since (zero: not playing)
	agentTo     time.Time
	lastAt      time.Time
	firedPause  time.Time // the pause (its lastVoicedAt) already given a sound
	guardUntil  time.Time
	held        [][]byte
	floor       float64 // the pause's level before the sound, RMS of PCM16
	replaying   bool
	plays       int
	stalePlays  int
	defaultUsed int
}

func (ms *ManagedStream) backchannelsOn() bool {
	return ms.orch != nil && ms.orch.config.Backchannels && !ms.clientVAD
}

// backchannelBankKey is the voice and language the reply is synthesised in.
func (ms *ManagedStream) backchannelVoiceLang() (Voice, Language) {
	voice := VoiceF1
	if ms.session != nil && ms.session.GetCurrentVoice() != "" {
		voice = ms.session.GetCurrentVoice()
	} else if ms.orch != nil && ms.orch.config.VoiceStyle != "" {
		voice = ms.orch.config.VoiceStyle
	}
	var lang Language
	if ms.session != nil {
		lang = ms.session.GetCurrentLanguage()
	}
	return voice, lang
}

// ensureBackchannelBank returns the bank for the call's current voice and language, loading it in the background
// when it is not there yet (nil until then: no sound).
func (ms *ManagedStream) ensureBackchannelBank() *BackchannelBank {
	prov, hasProv := ms.orch.tts.(BackchannelBankProvider)
	gen, canGen := ms.orch.tts.(VisemeTTSProvider)
	if !hasProv && !canGen {
		return nil
	}
	voice, lang := ms.backchannelVoiceLang()
	key := string(voice) + "|" + string(lang)
	ms.bc.mu.Lock()
	defer ms.bc.mu.Unlock()
	if ms.bc.bankKey == key && (ms.bc.bank != nil || time.Now().Before(ms.bc.bankRetry)) {
		return ms.bc.bank
	}
	if ms.bc.loading {
		return nil
	}
	rate := ms.playbackRate
	if rate <= 0 {
		return nil
	}
	if b := bcCacheGet(key + "|" + itoa(rate)); b != nil {
		ms.bc.bankKey, ms.bc.bank = key, b
		ms.logger.Info("Backchannels: sounds from this process's cache", "voice", voice, "lang", lang)
		return b
	}
	ms.bc.loading = true
	go func() {
		started := time.Now()
		var b *BackchannelBank
		var err error
		requests := 0
		if hasProv {
			ctx, cancel := context.WithTimeout(ms.ctx, backchannelBankTimeout)
			b, err = prov.BackchannelBank(ctx, voice, lang)
			cancel()
			if err == nil && b != nil && b.SampleRate > 0 && b.SampleRate != rate {
				b = resampleBank(b, rate)
			}
		} else {
			ctx, cancel := context.WithTimeout(WithBackgroundSynthesis(ms.ctx), backchannelGenTimeout)
			b, requests, err = generateBackchannelBank(ctx, gen, voice, lang, rate)
			cancel()
		}
		n := 0
		cats := []string{}
		if b != nil {
			for c, clips := range b.Clips {
				n += len(clips)
				cats = append(cats, c)
			}
		}
		if n == 0 {
			b = nil
		} else {
			bcCachePut(key+"|"+itoa(rate), b)
		}
		ms.bc.mu.Lock()
		ms.bc.loading = false
		ms.bc.bankKey, ms.bc.bank, ms.bc.bankRetry = key, b, time.Now().Add(backchannelBankRetry)
		ms.bc.mu.Unlock()
		if n == 0 {
			ms.logger.Info("Backchannels: no sounds for this call yet, none will play (asked again later)", "voice", voice,
				"lang", lang, "requests", requests, "ms", time.Since(started).Milliseconds(), "error", errString(err))
		} else {
			ms.logger.Info("Backchannels: sounds ready", "voice", voice, "lang", lang, "sounds", n, "categories", cats,
				"requests", requests, "ms", time.Since(started).Milliseconds())
		}
	}()
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// resampleBank converts every clip to rate (linear: these are short, band-limited sounds).
func resampleBank(b *BackchannelBank, rate int) *BackchannelBank {
	out := &BackchannelBank{SampleRate: rate, Clips: map[string][][]byte{}}
	for cat, clips := range b.Clips {
		for _, c := range clips {
			out.Clips[cat] = append(out.Clips[cat], resamplePCM16(c, b.SampleRate, rate))
		}
	}
	return out
}

func resamplePCM16(in []byte, from, to int) []byte {
	n := len(in) / 2
	if n == 0 || from == to {
		return in
	}
	m := int(int64(n) * int64(to) / int64(from))
	out := make([]byte, m*2)
	for i := 0; i < m; i++ {
		x := float64(i) * float64(from) / float64(to)
		j := int(x)
		f := x - float64(j)
		a := float64(int16(uint16(in[2*j]) | uint16(in[2*j+1])<<8))
		b := a
		if j+1 < n {
			b = float64(int16(uint16(in[2*j+2]) | uint16(in[2*j+3])<<8))
		}
		s := int16(math.Round(a + (b-a)*f))
		out[2*i], out[2*i+1] = byte(s), byte(uint16(s)>>8)
	}
	return out
}

// noteCallerText is new caller words for this turn: a pause's speculative transcript (final false) or an
// utterance's final transcript. The pick is refreshed in the background when the words have grown enough.
func (ms *ManagedStream) noteCallerText(text string, final bool) {
	if !ms.backchannelsOn() || ms.orch.GetLLMProvider() == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	ms.bc.mu.Lock()
	full := text
	if t := ms.bc.turnText; t != "" && !strings.HasPrefix(text, t) {
		full = t + " " + text
	}
	if final {
		ms.bc.turnText = full
	}
	ms.bc.latestText = full
	if ms.bc.inflight || (countWords(full)-countWords(ms.bc.pickText) < backchannelPickWords && ms.bc.pickText != "") ||
		time.Since(ms.bc.pickAsked) < backchannelPickGap {
		ms.bc.mu.Unlock()
		return
	}
	ms.bc.inflight, ms.bc.pickAsked = true, time.Now()
	ms.bc.mu.Unlock()
	go ms.refreshBackchannelPick(full)
}

func (ms *ManagedStream) refreshBackchannelPick(text string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(BackchannelContext(ms.ctx), backchannelPickTimeout)
	defer cancel()
	_, lang := ms.backchannelVoiceLang()
	raw, err := ms.orch.GetLLMProvider().Complete(ctx, BackchannelPickMessages(lang, ms.lastAgentLine(), text), nil)
	pick := ParseBackchannelPick(raw)
	ms.bc.mu.Lock()
	ms.bc.inflight = false
	if err == nil && pick != "" {
		ms.bc.pick, ms.bc.pickText, ms.bc.pickAt = pick, text, time.Now()
	}
	ms.bc.mu.Unlock()
	if err != nil || pick == "" {
		ms.logger.Info("Backchannel pick failed, keeping the previous one", "error", errString(err), "answer", ContentForLog(raw),
			"ms", time.Since(started).Milliseconds())
		return
	}
	ms.logger.Info("Backchannel pick", "pick", pick, "ms", time.Since(started).Milliseconds(), "words", countWords(text))
}

// lastAgentLine is the agent's most recent message in the conversation, or "".
func (ms *ManagedStream) lastAgentLine() string {
	if ms.session == nil {
		return ""
	}
	h := ms.session.GetContextCopy()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "assistant" && strings.TrimSpace(h[i].Content) != "" {
			return strings.TrimSpace(h[i].Content)
		}
	}
	return ""
}

// backchannelAudioIn runs first on every microphone chunk (server VAD). It counts the caller's speech, fires a
// backchannel at a pause, and while one plays hands the rest of handleAudio silence in place of the microphone.
// The returned chunk is what handleAudio goes on with. replay holds kept caller audio to process first.
func (ms *ManagedStream) backchannelAudioIn(chunk []byte, now time.Time) (out []byte, replay [][]byte) {
	if !ms.backchannelsOn() || len(chunk) == 0 {
		return chunk, nil
	}
	ms.bc.mu.Lock()
	if ms.bc.replaying {
		ms.bc.mu.Unlock()
		return chunk, nil
	}
	if now.Before(ms.bc.guardUntil) {
		if len(ms.bc.held)*len(chunk) < int(backchannelMaxHeld.Seconds()*float64(ms.inputSampleRate))*2 {
			ms.bc.held = append(ms.bc.held, append([]byte(nil), chunk...))
		}
		ms.bc.mu.Unlock()
		return make([]byte, len(chunk)), nil
	}
	if !ms.bc.guardUntil.IsZero() {
		// The window has just closed: the caller's, or the sound's echo?
		held, floor := ms.bc.held, ms.bc.floor
		ms.bc.held, ms.bc.guardUntil = nil, time.Time{}
		ms.bc.mu.Unlock()
		tail := append(lastBytes(held, int(ms.inputSampleRate)/10*2), chunk...)
		level := rmsPCM16(tail)
		if level > math.Max(4*floor, 120) {
			ms.logger.Info("Backchannel: the caller talked over the sound, their audio kept",
				"held_ms", heldMs(held, int(ms.inputSampleRate)), "level", int(level), "floor", int(floor))
			return chunk, held
		}
		if len(held) > 0 {
			ms.logger.Info("Backchannel: window closed, no caller speech in it (its echo, if any, dropped)",
				"held_ms", heldMs(held, int(ms.inputSampleRate)), "level", int(level), "floor", int(floor))
		}
		return chunk, nil
	}
	ms.bc.mu.Unlock()
	return chunk, nil
}

// backchannelAfterVAD runs after the VAD has read the chunk: it counts speech and fires at a pause.
func (ms *ManagedStream) backchannelAfterVAD(chunk []byte, isSpeaking bool, now time.Time) {
	if !ms.backchannelsOn() {
		return
	}
	// The bank is asked for from the call's first audio (built on the synthesis pool if no one has it yet), so it
	// is usually there before the caller's first long turn; until it lands nothing plays.
	ms.ensureBackchannelBank()
	ms.mu.Lock()
	playing := ms.playout.end.After(now) // the agent's audio is playing to the caller
	pending := ms.pendingBargeIn
	voiced := ms.lastVoicedAt
	rate := int(ms.inputSampleRate)
	ms.mu.Unlock()
	silent := true
	if sf, ok := ms.vad.(silenceFramesProvider); ok && isSpeaking && sf.SilenceFrames() == 0 {
		silent = false
	}
	ms.bc.mu.Lock()
	// The caller's turn starts over only once the agent has been HEARD: its audio played for backchannelHeardReset.
	// A reply discarded before playback, or one the caller cut off within that, never reached them and leaves the
	// count alone (2026-11-19: replies started in every mid-thought pause and cut off reset it each time, so a
	// backchannel was only ever reached at a real turn end).
	if playing {
		if ms.bc.agentFrom.IsZero() {
			ms.bc.agentFrom = now
		}
		ms.bc.agentTo = now
	} else if !ms.bc.agentFrom.IsZero() {
		if ms.bc.agentTo.Sub(ms.bc.agentFrom) >= backchannelHeardReset {
			ms.bc.speech, ms.bc.turnText, ms.bc.latestText, ms.bc.pick, ms.bc.pickText = 0, "", "", "", ""
		}
		ms.bc.agentFrom, ms.bc.agentTo = time.Time{}, time.Time{}
	}
	if !silent && rate > 0 {
		ms.bc.speech += time.Duration(len(chunk)/2) * time.Second / time.Duration(rate)
		ms.bc.mu.Unlock()
		return
	}
	pause := now.Sub(voiced)
	if voiced.IsZero() || pause < backchannelPauseDelay || ms.bc.firedPause.Equal(voiced) {
		ms.bc.mu.Unlock()
		return
	}
	// This pause is considered once: it either plays or says why not.
	ms.bc.firedPause = voiced
	reason := ""
	switch {
	case playing:
		reason = "agent_audio_playing"
	case pending:
		reason = "caller_over_agent"
	case pause > backchannelLatePause:
		reason = "pause_too_long"
	case ms.bc.speech < backchannelMinSpeech:
		reason = "too_little_speech"
	case !ms.bc.lastAt.IsZero() && now.Sub(ms.bc.lastAt) < backchannelMinInterval:
		reason = "too_soon_after_last"
	}
	speechMs, sinceLast := ms.bc.speech.Milliseconds(), int64(-1)
	if !ms.bc.lastAt.IsZero() {
		sinceLast = now.Sub(ms.bc.lastAt).Milliseconds()
	}
	pick, pickText, latest := ms.bc.pick, ms.bc.pickText, ms.bc.latestText
	ms.bc.mu.Unlock()
	skip := func(why string) {
		ms.logger.Info("Backchannel not played", "reason", why, "pause_ms", pause.Milliseconds(), "speech_ms", speechMs,
			"since_last_ms", sinceLast, "pick", pick)
	}
	if reason != "" {
		skip(reason)
		return
	}

	bank := ms.ensureBackchannelBank()
	if bank == nil {
		skip("no_sounds_yet")
		return
	}
	cat := pick
	if cat == "" {
		cat = BackchannelNeutral
	}
	if cat == BackchannelNone {
		skip("pick_none")
		ms.bc.mu.Lock()
		ms.bc.lastAt, ms.bc.speech = now, 0
		ms.bc.mu.Unlock()
		return
	}
	clips := bank.Clips[cat]
	if len(clips) == 0 {
		cat, clips = BackchannelNeutral, bank.Clips[BackchannelNeutral]
	}
	if len(clips) == 0 {
		skip("no_sound_for_category")
		return
	}
	ms.bc.mu.Lock()
	clip := clips[rand.Intn(len(clips))]
	if len(clips) > 1 && sameBytes(clip, ms.bc.lastClip) {
		for _, c := range clips {
			if !sameBytes(c, ms.bc.lastClip) {
				clip = c
				break
			}
		}
	}
	stale := pick != "" && pickText != latest
	ms.bc.lastAt, ms.bc.speech, ms.bc.lastClip = now, 0, clip
	ms.bc.plays++
	if stale {
		ms.bc.stalePlays++
	}
	if pick == "" {
		ms.bc.defaultUsed++
	}
	ms.bc.floor = rmsPCM16(ms.pauseAudio(rate))
	ms.bc.mu.Unlock()
	if !ms.emitBackchannelAudio(clip) {
		ms.logger.Info("Backchannel not played: stream closed")
		return
	}
	ms.logger.Info("Backchannel played", "category", cat, "pick", pick, "pick_stale", stale,
		"pick_age_ms", ageMs(ms.bc.pickAt, now, pick), "pause_ms", pause.Milliseconds(),
		"audio_ms", ms.pcmDuration(clip).Milliseconds())
}

func ageMs(at, now time.Time, pick string) int64 {
	if pick == "" || at.IsZero() {
		return -1
	}
	return now.Sub(at).Milliseconds()
}

// pauseAudio is the last ~200 ms of microphone audio (the pause so far), for its level.
func (ms *ManagedStream) pauseAudio(rate int) []byte {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	b := ms.preSpeechBuf.Bytes()
	n := rate / 5 * 2
	if n <= 0 || len(b) <= n {
		return append([]byte(nil), b...)
	}
	return append([]byte(nil), b[len(b)-n:]...)
}

func lastBytes(chunks [][]byte, n int) []byte {
	var out []byte
	for i := len(chunks) - 1; i >= 0 && len(out) < n; i-- {
		out = append(append([]byte(nil), chunks[i]...), out...)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func heldMs(chunks [][]byte, rate int) int {
	n := 0
	for _, c := range chunks {
		n += len(c)
	}
	if rate <= 0 {
		return 0
	}
	return n * 1000 / (2 * rate)
}

func rmsPCM16(b []byte) float64 {
	n := len(b) / 2
	if n == 0 {
		return 0
	}
	var s float64
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8))
		s += v * v
	}
	return math.Sqrt(s / float64(n))
}

func sameBytes(a, b []byte) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

func (ms *ManagedStream) pcmDuration(pcm []byte) time.Duration {
	rate := ms.playbackRate
	if rate <= 0 {
		rate = 44100
	}
	return time.Duration(len(pcm)/2) * time.Second / time.Duration(rate)
}

// emitBackchannelAudio sends the sound to the transport outside the reply path (not a turn: the stream's state is
// not touched), registers it as the agent's own audio for the echo check and Turno, opens the window in which the
// microphone is held back, and marks where a reply's audio may start (backchannelEnd). False when closed.
func (ms *ManagedStream) emitBackchannelAudio(pcm []byte) bool {
	if ms.isClosed.Load() || len(pcm) == 0 || ms.listeningTo() != nil { // listening: the agent makes no sound
		return false
	}
	now := time.Now()
	dur := ms.pcmDuration(pcm)
	ms.mu.Lock()
	gen := ms.payloadGen
	ms.backchannelEnd = now.Add(dur)
	ms.mu.Unlock()
	far := pcm
	if ms.playbackRate > 0 && ms.playbackRate != 16000 {
		far = resampleTo16k(pcm, ms.playbackRate)
	}
	ms.noteFarEndAudio(far)
	ms.noteFarEndEchoScheduled(gen, far, now)
	ms.bc.mu.Lock()
	ms.bc.guardUntil, ms.bc.held = now.Add(dur+backchannelEchoTail), nil
	ms.bc.mu.Unlock()

	frameSize := int(float64(ms.playbackRate)*0.06) * 2
	if frameSize <= 0 {
		frameSize = 5292
	}
	ms.eventsMu.Lock()
	defer ms.eventsMu.Unlock()
	if ms.isClosed.Load() {
		return false
	}
	for i := 0; i < len(pcm); i += frameSize {
		end := i + frameSize
		if end > len(pcm) {
			end = len(pcm)
		}
		c := make([]byte, end-i)
		copy(c, pcm[i:end])
		select {
		case ms.events <- OrchestratorEvent{Type: AudioChunk, Data: c, Generation: gen}:
		default:
			ms.logger.Warn("Backchannel audio dropped: event channel full", "bytes", len(pcm)-i)
			return true
		}
	}
	return true
}
