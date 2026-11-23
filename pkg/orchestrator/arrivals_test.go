package orchestrator

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// arriverTTS is a synthesis pool with arrivals: a stream speaks its sentences chunk by chunk, and a sentence that
// arrives while it is open is spoken by it after what it already has.
type arriverTTS struct {
	mu       sync.Mutex
	enabled  bool
	refuse   func(attempt int, text string) bool
	streams  [][]string
	open     int
	attempts int
	// afterCancel counts Arrive calls made with a cancelled context (there must be none).
	afterCancel atomic.Int32
	perSentence int           // chunks per sentence
	chunk       int           // bytes per chunk
	delay       time.Duration // between chunks
	started     chan struct{} // closed at the first chunk of the first stream
	startOnce   sync.Once
}

func newArriverTTS(enabled bool) *arriverTTS {
	return &arriverTTS{enabled: enabled, open: -1, perSentence: 4, chunk: 3200, delay: 5 * time.Millisecond,
		started: make(chan struct{})}
}

func (a *arriverTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return make([]byte, a.chunk), nil
}

func (a *arriverTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	a.mu.Lock()
	a.streams = append(a.streams, []string{text})
	idx := len(a.streams) - 1
	a.open = idx
	a.mu.Unlock()
	emitted := 0
	for {
		a.mu.Lock()
		total := len(a.streams[idx]) * a.perSentence
		if emitted >= total || ctx.Err() != nil {
			a.open = -1
			a.mu.Unlock()
			return nil
		}
		a.mu.Unlock()
		if err := onChunk(make([]byte, a.chunk)); err != nil {
			return err
		}
		a.startOnce.Do(func() { close(a.started) })
		emitted++
		time.Sleep(a.delay)
	}
}

func (a *arriverTTS) Abort() error { return nil }
func (a *arriverTTS) Name() string { return "arriverTTS" }

func (a *arriverTTS) ArrivalsEnabled() bool { return a.enabled }

func (a *arriverTTS) Arrive(ctx context.Context, text string, voice Voice, lang Language) bool {
	if ctx.Err() != nil {
		a.afterCancel.Add(1)
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts++
	if a.open < 0 || (a.refuse != nil && a.refuse(a.attempts, text)) {
		return false
	}
	a.streams[a.open] = append(a.streams[a.open], text)
	return true
}

func (a *arriverTTS) snapshot() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]string, len(a.streams))
	for i, s := range a.streams {
		out[i] = append([]string(nil), s...)
	}
	return out
}

func runReply(t *testing.T, tts TTSProvider, chunks []string) *ManagedStream {
	t.Helper()
	llm := &chunkedLLM{script: [][]string{chunks}}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("arrivals"))
	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(context.Background(), "hola"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reply did not finish")
	}
	return ms
}

const (
	s1 = "Te lo miro ahora mismo en el sistema."
	s2 = "Tengo un hueco mañana a las diez."
	s3 = "También hay otro el jueves por la tarde."
	s4 = "¿Cuál le viene mejor de los dos?"
)

func flat(streams [][]string) []string {
	var out []string
	for _, s := range streams {
		out = append(out, s...)
	}
	return out
}

// Every later sentence joins the stream that is speaking the reply: one synthesis for the whole reply, in order.
func TestArrivals_LaterSentencesJoinTheRunningStream(t *testing.T) {
	tts := newArriverTTS(true)
	ms := runReply(t, tts, []string{s1 + " ", s2 + " ", s3 + " ", s4})
	defer ms.Close()
	got := tts.snapshot()
	if len(got) != 1 || strings.Join(got[0], "|") != strings.Join([]string{s1, s2, s3, s4}, "|") {
		t.Fatalf("expected one stream carrying all four sentences in order, got %q", got)
	}
}

// A refused sentence is spoken as its own synthesis, and every sentence after it waits for that synthesis (whose stream
// may take them): the order never changes.
func TestArrivals_RefusedSentenceOpensTheNextStreamInOrder(t *testing.T) {
	tts := newArriverTTS(true)
	tts.refuse = func(attempt int, text string) bool { return text == s3 }
	ms := runReply(t, tts, []string{s1 + " ", s2 + " ", s3 + " ", s4})
	defer ms.Close()
	got := tts.snapshot()
	if strings.Join(flat(got), "|") != strings.Join([]string{s1, s2, s3, s4}, "|") {
		t.Fatalf("order changed: %q", got)
	}
	if len(got) != 2 || got[1][0] != s3 || len(got[1]) != 2 {
		t.Fatalf("expected [[s1 s2] [s3 s4]], got %q", got)
	}
}

// Refused every time: exactly one synthesis per sentence, as without arrivals.
func TestArrivals_AllRefusedIsOneSynthesisPerSentence(t *testing.T) {
	tts := newArriverTTS(true)
	tts.refuse = func(int, string) bool { return true }
	ms := runReply(t, tts, []string{s1 + " ", s2 + " ", s3})
	defer ms.Close()
	got := tts.snapshot()
	if len(got) != 3 || strings.Join(flat(got), "|") != strings.Join([]string{s1, s2, s3}, "|") {
		t.Fatalf("expected three streams in order, got %q", got)
	}
}

// Off: never asked to arrive, one synthesis per sentence, as today.
func TestArrivals_OffIsTodaysPath(t *testing.T) {
	tts := newArriverTTS(false)
	ms := runReply(t, tts, []string{s1 + " ", s2 + " ", s3})
	defer ms.Close()
	got := tts.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected one stream per sentence with arrivals off, got %q", got)
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if tts.attempts != 0 {
		t.Fatalf("Arrive called %d times with arrivals off", tts.attempts)
	}
}

// A short opener is still spoken with the sentence after it; the rest arrive into that stream.
func TestArrivals_ShortOpenerStillMerged(t *testing.T) {
	tts := newArriverTTS(true)
	ms := runReply(t, tts, []string{"Vale. ", s2 + " ", s3})
	defer ms.Close()
	got := tts.snapshot()
	if len(got) != 1 || got[0][0] != "Vale. "+s2 || len(got[0]) != 2 || got[0][1] != s3 {
		t.Fatalf("expected [[\"Vale. s2\" s3]], got %q", got)
	}
}

// A barge-in mid-stream stops the feeder: nothing arrives after the cancel, and the turn ends.
func TestArrivals_BargeInStopsTheFeeder(t *testing.T) {
	tts := newArriverTTS(true)
	tts.perSentence = 200 // a long stream
	tts.refuse = func(attempt int, text string) bool { return attempt > 1 }
	llm := &chunkedLLM{script: [][]string{{s1 + " ", s2 + " "}}}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("arrivals-barge"))
	defer ms.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(ctx, "hola"); close(done) }()
	select {
	case <-tts.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(30 * time.Millisecond)
	ms.handleInterrupt()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not end after the barge-in")
	}
	time.Sleep(50 * time.Millisecond)
	if n := tts.afterCancel.Load(); n != 0 {
		t.Fatalf("%d arrivals attempted after the cancel", n)
	}
}

// Spoken truth across one stream of two sentences: an interrupt partway into the second reports the first whole and
// the heard part of the second, and a stream that ended with a sentence the estimate never reached records it too.
func TestArrivals_SpokenTruthSplitsTheStreamBySentence(t *testing.T) {
	ms := &ManagedStream{playbackRate: 16000}
	gen := 3
	a := "uno dos tres cuatro cinco seis siete ocho nueve diez" // 51 chars ~ 3.4 s at 15/s
	b := "once doce trece catorce quince dieciseis diecisiete"
	f := &arrivalFeed{texts: []string{a, b}}
	t0 := time.Unix(1000, 0)
	ms.mu.Lock()
	ms.beginSegmentLocked(gen, a)
	// 6 s of audio in 100 ms chunks, handed to the transport at once (they queue back to back)
	n := 2 * 16000 / 10
	for i := 0; i < 60; i++ {
		f.attributeLocked(ms, gen, n)
		ms.scheduleAudioLocked(gen, n, t0)
	}
	text, _, complete := ms.playout.heard(t0.Add(4500 * time.Millisecond))
	ms.mu.Unlock()
	if complete || !strings.HasPrefix(text, a+" once") || strings.HasSuffix(text, "diecisiete") {
		t.Fatalf("interrupted in the second sentence: got %q complete=%v", text, complete)
	}

	// A third sentence that arrived but whose estimated start the audio never reached: added to the last segment.
	ms2 := &ManagedStream{playbackRate: 16000}
	c := "y una frase final bastante larga que no llega a empezar en la estimación"
	f2 := &arrivalFeed{texts: []string{a, c}}
	ms2.mu.Lock()
	ms2.beginSegmentLocked(gen, a)
	for i := 0; i < 20; i++ { // 2 s: still inside a's estimate
		f2.attributeLocked(ms2, gen, n)
		ms2.scheduleAudioLocked(gen, n, t0)
	}
	f2.settleLocked(ms2, gen)
	text, _, complete = ms2.playout.heard(t0.Add(10 * time.Second))
	ms2.mu.Unlock()
	if !complete || text != a+" "+c {
		t.Fatalf("settled stream: got %q complete=%v", text, complete)
	}
}

func TestSentenceSource_PushFrontKeepsOrder(t *testing.T) {
	q := make(chan string, 4)
	q <- "a"
	q <- "b"
	close(q)
	s := newSentenceSource(q)
	x, _ := s.pop(nil)
	s.pushFront(x)
	var got []string
	for {
		v, ok := s.pop(nil)
		if !ok {
			break
		}
		got = append(got, v)
	}
	if strings.Join(got, "") != "ab" {
		t.Fatalf("got %v", got)
	}
	stop := make(chan struct{})
	close(stop)
	open := newSentenceSource(make(chan string))
	if _, ok := open.pop(stop); ok {
		t.Fatal("pop returned a sentence from an empty open source")
	}
}
