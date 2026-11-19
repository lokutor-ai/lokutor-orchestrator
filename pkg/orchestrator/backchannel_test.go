package orchestrator

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// bcVAD is a VAD whose speech and silent-frame count the test sets.
type bcVAD struct {
	mu       sync.Mutex
	speaking bool
	silent   int
}

func (v *bcVAD) Process(chunk []byte) (*VADEvent, error) { return nil, nil }
func (v *bcVAD) IsSpeaking() bool                        { v.mu.Lock(); defer v.mu.Unlock(); return v.speaking }
func (v *bcVAD) Reset()                                  {}
func (v *bcVAD) Clone() VADProvider                      { return v }
func (v *bcVAD) Name() string                            { return "bc" }
func (v *bcVAD) SilenceFrames() int                      { v.mu.Lock(); defer v.mu.Unlock(); return v.silent }

// bcTTS is a TTS provider with a bank: 300 ms clips at 16 kHz, a different level per category so a test can tell
// which played.
type bcTTS struct {
	MockTTSProvider
	loads int
	mu    sync.Mutex
}

func bcClip(level int16, ms int) []byte {
	n := 16 * ms
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := level
		if i%2 == 1 {
			v = -level
		}
		b[2*i], b[2*i+1] = byte(v), byte(uint16(v)>>8)
	}
	return b
}

func (t *bcTTS) BackchannelBank(ctx context.Context, voice Voice, lang Language) (*BackchannelBank, error) {
	t.mu.Lock()
	t.loads++
	t.mu.Unlock()
	return &BackchannelBank{SampleRate: 16000, Clips: map[string][][]byte{
		BackchannelNeutral: {bcClip(1000, 300), bcClip(1001, 300)}, BackchannelSympathy: {bcClip(3000, 400)},
	}}, nil
}

// bcPickLLM answers the pick request with answer and records how it was asked.
type bcPickLLM struct {
	mu        sync.Mutex
	answer    string
	calls     int
	category  string
	maxTokens int
}

func (l *bcPickLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.category, l.maxTokens = TokenCategoryFrom(ctx), MaxTokensFrom(ctx)
	return l.answer, nil
}
func (l *bcPickLLM) Name() string { return "bcPick" }

func resetBCCache() {
	bcCacheMu.Lock()
	bcCache = map[string]bcCacheEntry{}
	bcCacheMu.Unlock()
}

func bcStream(t *testing.T, llm LLMProvider) (*ManagedStream, *bcVAD) {
	t.Helper()
	resetBCCache()
	cfg := DefaultConfig()
	cfg.Backchannels = true
	cfg.SilenceTimeout = 0
	vad := &bcVAD{}
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hi"}, llm, &bcTTS{}, vad, cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("bc"))
	ms.SetPlaybackRate(16000)
	ms.inputSampleRate = 16000
	ms.vad = vad
	t.Cleanup(ms.Close)
	// the bank loads in the background on first use
	ms.ensureBackchannelBank()
	for i := 0; i < 100 && ms.ensureBackchannelBank() == nil; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	return ms, vad
}

// readyPause puts the stream 300 ms into a pause after enough caller speech.
func readyPause(ms *ManagedStream, vad *bcVAD, pause time.Duration) time.Time {
	now := time.Now()
	vad.mu.Lock()
	vad.speaking, vad.silent = true, int(pause/(32*time.Millisecond))
	vad.mu.Unlock()
	ms.mu.Lock()
	ms.lastVoicedAt = now.Add(-pause)
	ms.mu.Unlock()
	ms.bc.mu.Lock()
	ms.bc.speech = 2500 * time.Millisecond
	ms.bc.mu.Unlock()
	return now
}

func bcAudio(ms *ManagedStream, within time.Duration) []byte {
	deadline := time.After(within)
	var got []byte
	for {
		select {
		case e := <-ms.Events():
			if e.Type == AudioChunk {
				got = append(got, e.Data.([]byte)...)
			}
		case <-deadline:
			return got
		}
	}
}

func TestBackchannelPlaysAQuarterSecondIntoAPause(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	ms.session.AddMessage("assistant", "Hi, how can I help?")
	before := len(ms.session.GetContextCopy())
	// 120 ms in: too early.
	now := readyPause(ms, vad, 120*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if a := bcAudio(ms, 30*time.Millisecond); len(a) != 0 {
		t.Fatal("a sound 120 ms into a pause")
	}
	now = readyPause(ms, vad, 260*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	a := bcAudio(ms, 50*time.Millisecond)
	if len(a) != 2*16*300 {
		t.Fatalf("%d bytes of audio, want one 300 ms neutral clip (no pick yet: neutral)", len(a))
	}
	// Once per pause.
	ms.backchannelAfterVAD(make([]byte, 640), true, now.Add(20*time.Millisecond))
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("two sounds in one pause")
	}
	if got := len(ms.session.GetContextCopy()); got != before {
		t.Fatalf("history changed: %d, was %d", got, before)
	}
	ms.mu.Lock()
	st, end := ms.state, ms.backchannelEnd
	ms.mu.Unlock()
	if st == StateSpeaking || !end.After(now) {
		t.Fatalf("state %v, backchannelEnd %v", st, end)
	}
	ms.echoMu.Lock()
	far := len(ms.echoFarPending)
	ms.echoMu.Unlock()
	if far == 0 {
		t.Fatal("not registered on the echo check's far end")
	}
	if ms.bc.defaultUsed != 1 || ms.bc.plays != 1 {
		t.Fatalf("plays %d default %d", ms.bc.plays, ms.bc.defaultUsed)
	}
}

func TestBackchannelLimits(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	// Too little speech.
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.bc.mu.Lock()
	ms.bc.speech = 1500 * time.Millisecond
	ms.bc.mu.Unlock()
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound after 1.5 s of speech")
	}
	// A pause long past: the moment is gone.
	now = readyPause(ms, vad, 900*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound 900 ms into a pause")
	}
	// The agent's audio is playing.
	now = readyPause(ms, vad, 300*time.Millisecond)
	ms.mu.Lock()
	ms.playout.end = now.Add(time.Second)
	ms.mu.Unlock()
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound over the agent's own audio")
	}
	ms.mu.Lock()
	ms.playout.end = time.Time{}
	ms.mu.Unlock()
	// One plays; the next pause within the interval does not.
	now = readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) == 0 {
		t.Fatal("no sound when every condition held")
	}
	time.Sleep(5 * time.Millisecond)
	now = readyPause(ms, vad, 300*time.Millisecond)
	ms.bc.mu.Lock()
	ms.bc.guardUntil = time.Time{}
	ms.bc.mu.Unlock()
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("two sounds within the interval")
	}
}

func TestBackchannelPlaysThePickAndNoneIsSilent(t *testing.T) {
	llm := &bcPickLLM{answer: "Sympathy."}
	ms, vad := bcStream(t, llm)
	ms.noteCallerText("my dad passed away last month and", false)
	for i := 0; i < 100; i++ {
		ms.bc.mu.Lock()
		p := ms.bc.pick
		ms.bc.mu.Unlock()
		if p != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if llm.category != TokensBackchannel || llm.maxTokens != backchannelMaxTokens {
		t.Fatalf("pick request category %q max tokens %d", llm.category, llm.maxTokens)
	}
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if a := bcAudio(ms, 50*time.Millisecond); len(a) != 2*16*400 {
		t.Fatalf("%d bytes, want the 400 ms sympathy clip", len(a))
	}
	ms.bc.mu.Lock()
	ms.bc.pick, ms.bc.lastAt, ms.bc.guardUntil = BackchannelNone, time.Time{}, time.Time{}
	ms.bc.mu.Unlock()
	now = readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound with the pick none")
	}
}

func TestBackchannelPickIsMarkedStaleWhenWordsMovedOn(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{answer: "neutral"})
	ms.bc.mu.Lock()
	ms.bc.pick, ms.bc.pickText, ms.bc.latestText, ms.bc.inflight = BackchannelNeutral, "so I called", "so I called and they said", true
	ms.bc.mu.Unlock()
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	bcAudio(ms, 30*time.Millisecond)
	if ms.bc.stalePlays != 1 {
		t.Fatalf("stale plays %d, want 1", ms.bc.stalePlays)
	}
}

func TestBackchannelWindowHidesItsEchoAndKeepsTheCaller(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	bcAudio(ms, 20*time.Millisecond)
	loud := bcClip(4000, 20)
	out, replay := ms.backchannelAudioIn(loud, time.Now())
	if rmsPCM16(out) != 0 || replay != nil {
		t.Fatal("microphone audio reached the VAD while the sound played")
	}
	// The echo fades in the window's tail; it closes on quiet audio: the echo is dropped.
	for i := 0; i < 5; i++ {
		ms.backchannelAudioIn(make([]byte, 640), time.Now())
	}
	ms.bc.mu.Lock()
	ms.bc.guardUntil = time.Now().Add(-time.Millisecond)
	ms.bc.mu.Unlock()
	out, replay = ms.backchannelAudioIn(make([]byte, 640), time.Now())
	if replay != nil || len(out) != 640 {
		t.Fatal("echo kept as caller speech")
	}
	// A caller still talking when it closes: their audio comes back, in order.
	ms.bc.mu.Lock()
	ms.bc.guardUntil, ms.bc.floor = time.Now().Add(40*time.Millisecond), 50
	ms.bc.mu.Unlock()
	ms.backchannelAudioIn(bcClip(2000, 20), time.Now())
	ms.backchannelAudioIn(bcClip(2001, 20), time.Now())
	time.Sleep(45 * time.Millisecond)
	out, replay = ms.backchannelAudioIn(loud, time.Now())
	if len(replay) != 2 || rmsPCM16(out) == 0 {
		t.Fatalf("caller audio over the sound: %d chunks replayed", len(replay))
	}
}

func TestReplyAudioIsScheduledAfterTheBackchannel(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	bcEnd := ms.backchannelEnd
	ms.scheduleAudioLocked(ms.payloadGen+1, 3200, time.Now())
	if want := bcEnd.Add(100 * time.Millisecond); ms.playout.end.Before(want) {
		t.Fatalf("reply playout ends %v, want after the backchannel (%v)", ms.playout.end, want)
	}
}

func TestBackchannelsOffByDefault(t *testing.T) {
	orch := NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, &bcTTS{}, &bcVAD{}, DefaultConfig())
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("off"))
	defer ms.Close()
	if ms.backchannelsOn() {
		t.Fatal("on by default")
	}
}

func TestParseBackchannelPick(t *testing.T) {
	for in, want := range map[string]string{"neutral": "neutral", "Sympathy.": "sympathy", " none\n": "none", "surprise - wow": "surprise",
		"mhm": "", "": "", "ack": "ack"} {
		if got := ParseBackchannelPick(in); got != want {
			t.Errorf("ParseBackchannelPick(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResampleBankKeepsDuration(t *testing.T) {
	b := resampleBank(&BackchannelBank{SampleRate: 44100, Clips: map[string][][]byte{"neutral": {make([]byte, 2*44100/2)}}}, 8000)
	if n := len(b.Clips["neutral"][0]) / 2; n != 4000 {
		t.Fatalf("%d samples at 8 kHz, want 4000", n)
	}
}

// The whole path in real time: 20 ms microphone chunks through handleAudio, 2.5 s of speech, then silence; the
// sound must start ~250 ms after the last voiced chunk (the trigger is checked on every chunk).
func TestBackchannelLatencyFromThePauseThroughHandleAudio(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	chunk := bcClip(800, 20)
	var lastVoiced time.Time
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	for time.Since(start) < 2500*time.Millisecond {
		<-tick.C
		vad.mu.Lock()
		vad.speaking, vad.silent = true, 0
		vad.mu.Unlock()
		ms.handleAudio(chunk)
		lastVoiced = time.Now()
	}
	silent := 0
	var heard time.Time
	for heard.IsZero() && time.Since(lastVoiced) < time.Second {
		<-tick.C
		silent++
		vad.mu.Lock()
		vad.silent = silent
		vad.mu.Unlock()
		ms.handleAudio(make([]byte, 640))
		select {
		case e := <-ms.Events():
			if e.Type == AudioChunk {
				heard = time.Now()
			}
		default:
		}
	}
	if heard.IsZero() {
		t.Fatal("no backchannel in the pause")
	}
	d := heard.Sub(lastVoiced)
	t.Logf("pause start -> backchannel audio handed to the transport: %d ms", d.Milliseconds())
	if d < 240*time.Millisecond || d > 300*time.Millisecond {
		t.Fatalf("%v from the pause to the sound, want ~250-270 ms", d)
	}
}

// gatedBankTTS answers the bank request only once release is closed (a first build on the synthesis pool).
type gatedBankTTS struct {
	MockTTSProvider
	release chan struct{}
	asked   chan struct{}
	once    sync.Once
}

func (g *gatedBankTTS) BackchannelBank(ctx context.Context, voice Voice, lang Language) (*BackchannelBank, error) {
	g.once.Do(func() { close(g.asked) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &BackchannelBank{SampleRate: 16000, Clips: map[string][][]byte{BackchannelNeutral: {bcClip(1000, 300)}}}, nil
}

// A session whose voice has no bank yet asks for it from its first audio, plays nothing meanwhile, and starts
// backchanneling as soon as it lands.
func TestBackchannelStartsWhenTheBankLands(t *testing.T) {
	resetBCCache()
	cfg := DefaultConfig()
	cfg.Backchannels, cfg.SilenceTimeout = true, 0
	vad := &bcVAD{}
	g := &gatedBankTTS{release: make(chan struct{}), asked: make(chan struct{})}
	orch := NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, g, vad, cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("late-bank"))
	defer ms.Close()
	ms.SetPlaybackRate(16000)
	ms.inputSampleRate = 16000
	ms.vad = vad
	ms.handleAudio(make([]byte, 640)) // the call's first audio asks for the bank
	select {
	case <-g.asked:
	case <-time.After(time.Second):
		t.Fatal("the bank was not asked for at the session's first audio")
	}
	now := readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound before the bank existed")
	}
	close(g.release)
	for i := 0; i < 100 && ms.ensureBackchannelBank() == nil; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	now = readyPause(ms, vad, 300*time.Millisecond)
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 50*time.Millisecond)) == 0 {
		t.Fatal("no sound once the bank had landed")
	}
}

// emptyThenBankTTS has no bank on the first request (still being built) and one on the next.
type emptyThenBankTTS struct {
	MockTTSProvider
	mu    sync.Mutex
	calls int
}

func (e *emptyThenBankTTS) BackchannelBank(ctx context.Context, voice Voice, lang Language) (*BackchannelBank, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.calls == 1 {
		return nil, nil
	}
	return &BackchannelBank{SampleRate: 16000, Clips: map[string][][]byte{BackchannelNeutral: {bcClip(1000, 300)}}}, nil
}

func TestBackchannelBankAskedAgainAfterNone(t *testing.T) {
	resetBCCache()
	cfg := DefaultConfig()
	cfg.Backchannels = true
	p := &emptyThenBankTTS{}
	orch := NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, p, &bcVAD{}, cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("retry"))
	defer ms.Close()
	ms.SetPlaybackRate(16000)
	ms.ensureBackchannelBank()
	for i := 0; i < 100; i++ {
		ms.bc.mu.Lock()
		l := ms.bc.loading
		ms.bc.mu.Unlock()
		if !l {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ms.ensureBackchannelBank() != nil {
		t.Fatal("a bank from an empty answer")
	}
	ms.bc.mu.Lock()
	ms.bc.bankRetry = time.Now().Add(-time.Millisecond)
	ms.bc.mu.Unlock()
	ms.ensureBackchannelBank()
	for i := 0; i < 100 && ms.ensureBackchannelBank() == nil; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if ms.ensureBackchannelBank() == nil {
		t.Fatal("the bank was not asked for again after the retry delay")
	}
}

// A reply the caller never heard (discarded, or cut off within a second) leaves their speech count; one they heard
// for a second or more starts it over.
func TestBackchannelCountSurvivesUnheardReplies(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	vad.mu.Lock()
	vad.speaking, vad.silent = true, 0
	vad.mu.Unlock()
	ms.bc.mu.Lock()
	ms.bc.speech = 2 * time.Second
	ms.bc.mu.Unlock()
	now := time.Now()
	// 500 ms of agent audio, then cut off.
	ms.mu.Lock()
	ms.playout.end = now.Add(500 * time.Millisecond)
	ms.mu.Unlock()
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	ms.backchannelAfterVAD(make([]byte, 640), true, now.Add(480*time.Millisecond))
	ms.backchannelAfterVAD(make([]byte, 640), true, now.Add(520*time.Millisecond))
	ms.bc.mu.Lock()
	sp := ms.bc.speech
	ms.bc.mu.Unlock()
	if sp < 2*time.Second {
		t.Fatalf("speech %v after an unheard reply, want the 2 s kept (plus the chunks)", sp)
	}
	// 1.5 s heard.
	now = now.Add(time.Second)
	ms.mu.Lock()
	ms.playout.end = now.Add(1500 * time.Millisecond)
	ms.mu.Unlock()
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	ms.backchannelAfterVAD(make([]byte, 640), true, now.Add(1400*time.Millisecond))
	ms.backchannelAfterVAD(make([]byte, 640), true, now.Add(1600*time.Millisecond))
	ms.bc.mu.Lock()
	sp = ms.bc.speech
	ms.bc.mu.Unlock()
	if sp > 100*time.Millisecond {
		t.Fatalf("speech %v after a heard reply, want it started over", sp)
	}
}

// With a model configured, a backchannel call runs the predictor on the caller's audio and logs its summary at close;
// a missing model leaves the call untouched.
func TestBackchannelShadowRunsOnlyWithAModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backchannels = true
	cfg.BackchannelPredictorPath = "/nonexistent/bc.onnx"
	orch := NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, &bcTTS{}, &bcVAD{}, cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("shadow-none"))
	if ms.bcShadow != nil {
		t.Fatal("a shadow predictor without a model")
	}
	ms.Close()
	path := os.Getenv("BC_PREDICTOR_TEST_MODEL")
	if path == "" {
		t.Skip("BC_PREDICTOR_TEST_MODEL not set")
	}
	cfg.BackchannelPredictorPath = path
	orch = NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, &bcTTS{}, &bcVAD{}, cfg)
	ms = orch.NewManagedStream(context.Background(), NewConversationSession("shadow"))
	if ms.bcShadow == nil {
		t.Fatal("no shadow predictor with a model")
	}
	ms.backchannelShadowAudio(bcClip(3000, 500), time.Now())
	if ms.bcShadow.frames != 25 {
		t.Fatalf("%d frames from 500 ms", ms.bcShadow.frames)
	}
	ms.Close()
}
