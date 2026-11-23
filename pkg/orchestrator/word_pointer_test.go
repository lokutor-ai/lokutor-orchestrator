package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

// pointerTTS is a word-pointer pool in process: a reply opened with a PointerReply is one stream that says the words of
// its text as they arrive (whole words only, as the pool lays them), one 100 ms chunk a word, and reports each word it
// finishes with where it ends in the reply's text; it ends once the end has arrived and every word is said.
type pointerTTS struct {
	mu      sync.Mutex
	enabled bool // the worker's switch and the pool's answer, as the provider reports them
	pointer bool // the pool answers as a word-pointer pool
	replies []*fakeReply
	plain   []string // synthesis without a PointerReply (today's path)
	// failAfter: reply number (1-based) -> words said before it fails with failReason
	failAfter  map[int]int
	failReason string
	// halfFail: in the first reply, the written word with this index is said as two spoken words, and the reply fails
	// after the first of them (the written word half said)
	halfFail int
	// refuse: an append the pool refuses for room (by reply number and append number, 1-based)
	refuseRoom map[[2]int]bool
	terms      []string
	rate       int // playback rate the chunks are sized for
	wordDelay  time.Duration
	started    chan struct{}
	startOnce  sync.Once
}

type fakeReply struct {
	id, continueFrom string
	open             bool
	opening          string
	appends          []string
	ends             []bool
	text             string // everything received
	ended            bool
	said             int // words said
	failed           string
	changed          chan struct{}
}

func newPointerTTS() *pointerTTS {
	return &pointerTTS{enabled: true, pointer: true, rate: 44100, wordDelay: 2 * time.Millisecond, halfFail: -1,
		failAfter: map[int]int{}, refuseRoom: map[[2]int]bool{}, started: make(chan struct{})}
}

func (p *pointerTTS) WordPointerEnabled() bool { return p.enabled }
func (p *pointerTTS) WordPointerTerms() []string {
	return p.terms
}
func (p *pointerTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return nil, nil
}
func (p *pointerTTS) Abort() error { return nil }
func (p *pointerTTS) Name() string { return "pointerTTS" }

func (p *pointerTTS) chunk() []byte { return make([]byte, p.rate*2/10) } // 100 ms

func (p *pointerTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	pr := PointerReplyFrom(ctx)
	p.mu.Lock()
	pointer := p.pointer
	if pr == nil {
		p.plain = append(p.plain, text)
	}
	p.mu.Unlock()
	if pr == nil || !pointer {
		if pr != nil {
			pr.Opened(false, false, nil)
			p.mu.Lock()
			p.replies = append(p.replies, &fakeReply{id: pr.ID, continueFrom: pr.ContinueFrom, open: pr.Open, opening: text})
			p.mu.Unlock()
		}
		for range strings.Fields(text) {
			if ctx.Err() != nil {
				return nil
			}
			if err := onChunk(p.chunk()); err != nil {
				return err
			}
		}
		return nil
	}
	r := &fakeReply{id: pr.ID, continueFrom: pr.ContinueFrom, open: pr.Open, opening: text, text: text, ended: !pr.Open,
		changed: make(chan struct{})}
	p.mu.Lock()
	p.replies = append(p.replies, r)
	n := len(p.replies)
	failAt, failing := p.failAfter[n]
	p.mu.Unlock()
	pr.Opened(true, pr.ContinueFrom != "", func(ctx context.Context, t string, end bool) (PointerVerdict, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.ended || r.failed != "" {
			return PointerVerdict{Reason: "ended", Room: -1}, nil
		}
		if p.refuseRoom[[2]int{n, len(r.appends) + 1}] && t != "" {
			delete(p.refuseRoom, [2]int{n, len(r.appends) + 1})
			return PointerVerdict{Reason: "capacity", Room: 10}, nil
		}
		r.appends = append(r.appends, t)
		r.ends = append(r.ends, end)
		r.text += t
		r.ended = r.ended || end
		close(r.changed)
		r.changed = make(chan struct{})
		return PointerVerdict{Arrived: true, Mode: "appended", Room: 1000}, nil
	})
	for {
		p.mu.Lock()
		words := spokenWords(r.text, r.ended)
		ch := r.changed
		if failing && r.said >= failAt {
			r.failed = p.failReason
			p.mu.Unlock()
			pr.Failed(p.failReason)
			return fmt.Errorf("versa2: stream read failed: unexpected EOF")
		}
		if n == 1 && r.said == p.halfFail && r.said < len(words) {
			idx := r.said
			p.mu.Unlock()
			_ = onChunk(p.chunk())
			pr.Word(PointerWord{Index: idx, EndMs: float64(idx+1) * 100, TextEnd: words[idx], TextStart: -1, WrittenDone: false})
			pr.Failed("watchdog")
			return fmt.Errorf("versa2: pointer reply failed (watchdog)")
		}
		if r.said < len(words) {
			w := words[r.said]
			r.said++
			idx := r.said - 1
			p.mu.Unlock()
			if ctx.Err() != nil {
				return nil
			}
			if err := onChunk(p.chunk()); err != nil {
				return err
			}
			p.startOnce.Do(func() { close(p.started) })
			pr.Word(PointerWord{Index: idx, StartMs: float64(idx) * 100, EndMs: float64(idx+1) * 100, TextEnd: w, TextStart: -1, WrittenDone: true})
			time.Sleep(p.wordDelay)
			continue
		}
		if r.ended {
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			pr.Failed("text_stalled")
			return fmt.Errorf("versa2: stream read failed: unexpected EOF")
		}
	}
}

// spokenWords are the rune ends of the words of text the pool can say: every word followed by whitespace, and the last
// one too once the end has arrived.
func spokenWords(text string, ended bool) []int {
	var out []int
	rs := []rune(text)
	in := false
	for i, r := range rs {
		if unicode.IsSpace(r) {
			if in {
				out = append(out, i)
			}
			in = false
		} else {
			in = true
		}
	}
	if in && ended {
		out = append(out, len(rs))
	}
	return out
}

func (p *pointerTTS) snapshot() ([]fakeReply, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]fakeReply, len(p.replies))
	for i, r := range p.replies {
		out[i] = *r
	}
	return out, append([]string(nil), p.plain...)
}

func runPointerTurn(t *testing.T, tts *pointerTTS, llm StreamingLLMProvider, log Logger) *ManagedStream {
	t.Helper()
	if log == nil {
		log = &NoOpLogger{}
	}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), log)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("pointer"))
	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(context.Background(), "hola"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reply did not finish")
	}
	return ms
}

// pacedLLM writes its script's chunks with a pause between them, as a model streams.
type pacedLLM struct {
	chunkedLLM
	delay time.Duration
}

func streamed(delay time.Duration, chunks ...string) *pacedLLM {
	return &pacedLLM{chunkedLLM: chunkedLLM{script: [][]string{chunks}}, delay: delay}
}

func (m *pacedLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	return m.chunkedLLM.StreamComplete(ctx, messages, tools, func(c string) error {
		time.Sleep(m.delay)
		return onChunk(c)
	}, onToolCall)
}

func joined(r fakeReply) string { return r.opening + strings.Join(r.appends, "") }

const pointerReply = "Claro, te lo miro ahora mismo en el sistema. Tengo un hueco mañana a las 10:30 a.m. con el Sr. Pérez, si le va bien."

// The reply is one stream: opened with its first words, every later piece appended as the LLM writes it, ending at a
// word boundary, never splitting a time, an abbreviation or the word being written; the end with the last piece.
func TestPointer_TheReplyIsOneStreamFedWordByWord(t *testing.T) {
	tts := newPointerTTS()
	chunks := []string{"Claro, te ", "lo miro ah", "ora mismo en el sistema. ", "Tengo un hueco mañana a las 10", ":30 ", "a.m. con el ",
		"Sr. ", "Pérez, si le ", "va bien."}
	ms := runPointerTurn(t, tts, streamed(15*time.Millisecond, chunks...), nil)
	defer ms.Close()
	replies, plain := tts.snapshot()
	if len(replies) != 1 || len(plain) != 0 {
		t.Fatalf("want one word-pointer reply and no sentence synthesis, got %d replies, plain %q", len(replies), plain)
	}
	r := replies[0]
	if got := joined(r); got != pointerReply {
		t.Fatalf("the reply's text as the pool received it:\n got %q\nwant %q", got, pointerReply)
	}
	if !r.open || r.continueFrom != "" {
		t.Fatalf("opened open=%v continue_from=%q", r.open, r.continueFrom)
	}
	if n := len(strings.Fields(r.opening)); n < pointerOpenWords && !strings.Contains(r.opening, ". ") {
		t.Fatalf("opened with %q: fewer than %d words and no sentence", r.opening, pointerOpenWords)
	}
	if len(r.appends) < 2 {
		t.Fatalf("expected the words to arrive in several appends, got %q", r.appends)
	}
	for i, a := range r.appends {
		last := i == len(r.appends)-1
		if r.ends[i] != last {
			t.Fatalf("append %d end=%v (last=%v): %q", i, r.ends[i], last, r.appends)
		}
		if !last && !strings.HasSuffix(a, " ") {
			t.Fatalf("append %d does not end at a word boundary: %q", i, a)
		}
	}
	pieces := append([]string{r.opening}, r.appends...)
	for _, p := range pieces[:len(pieces)-1] {
		for _, bad := range []string{"10:30 ", "a.m. ", "Sr. ", "ah", "las "} {
			if strings.HasSuffix(p, bad) && bad != "las " {
				t.Fatalf("a piece ended after %q, whose reading may still change: %q", bad, pieces)
			}
		}
	}
	// the record of the turn is the LLM's text, as without the pointer
	ctxMsgs := ms.session.GetContextCopy()
	if got := ctxMsgs[len(ctxMsgs)-1]; got.Role != "assistant" || got.Content != pointerReply {
		t.Fatalf("recorded %+v", got)
	}
}

func TestPointerSafeEnd(t *testing.T) {
	cases := []struct {
		in    string
		guard int
		want  string
	}{
		{"Claro, te lo mi", 0, "Claro, te lo "},
		{"Claro, te lo mi", 2, "Claro, "},
		{"a las 10:30 ", 0, "a las "},
		{"a las 10:30 a.m. ", 0, "a las "},
		{"a las 10:30 a.m. nos ", 0, "a las 10:30 a.m. nos "},
		{"con el Sr. ", 0, "con el "},
		{"con el Sr. Pérez ", 0, "con el Sr. Pérez "},
		{"cuesta 3.", 0, "cuesta "},
		{"cuesta 3.5 ", 0, "cuesta "},
		{"cuesta 3.5 km. Y ", 0, "cuesta 3.5 km. Y "},
		{"son $ ", 0, "son "},
		{"y [laugh] ", 0, "y "},
		{"vale", 0, ""},
		{"", 0, ""},
		{"todo, ¿vale? ", 0, "todo, ¿vale? "},
	}
	for _, c := range cases {
		if got := c.in[:pointerSafeEnd(c.in, false, nil, c.guard)]; got != c.want {
			t.Errorf("pointerSafeEnd(%q, guard %d) = %q, want %q", c.in, c.guard, got, c.want)
		}
	}
	if got := pointerSafeEnd("a las 10:30", true, nil, 2); got != len("a las 10:30") {
		t.Errorf("the end lays everything, got %d", got)
	}
	terms := []string{"Lokutor Cloud Platform", "API"}
	for in, want := range map[string]string{
		"usa Lokutor ":                   "usa ",
		"usa Lokutor Cloud ":             "usa ",
		"usa Lokutor Cloud Platform ya ": "usa Lokutor Cloud Platform ya ",
		"usa Lokutor y nada más ":        "usa Lokutor y nada más ",
		"la API ":                        "la API ",
	} {
		if got := in[:pointerSafeEnd(in, false, terms, 0)]; got != want {
			t.Errorf("terms: %q -> %q, want %q", in, got, want)
		}
	}
}

// A tool call ends the reply after the words said before it; the reply after the tool is a new one that continues it.
func TestPointer_ToolCallClosesTheReplyAndTheNextContinuesIt(t *testing.T) {
	llm := &MockStreamingLLM{responses: []struct {
		content   string
		toolCalls []ToolCallEventData
	}{
		{content: "Un momento, que lo miro en el sistema.", toolCalls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}}},
		{content: "Ya está: tiene la cita el jueves a las cinco. ¿Algo más?"},
	}}
	tts := newPointerTTS()
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	orch.RegisterTool("lookup", func(args string) (string, error) { return `{"ok":true}`, nil })
	session := NewConversationSession("pointer-tool")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()
	ms.runLLMAndTTS(context.Background(), "mira mi cita")
	deadline := time.Now().Add(5 * time.Second)
	var replies []fakeReply
	for time.Now().Before(deadline) {
		replies, _ = tts.snapshot()
		if len(replies) == 2 && replies[1].said > 0 && replies[1].said == len(strings.Fields(joined(replies[1]))) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(replies) != 2 {
		t.Fatalf("want two replies (before and after the tool), got %d: %+v", len(replies), replies)
	}
	if got := strings.TrimSpace(joined(replies[0])); got != "Un momento, que lo miro en el sistema." {
		t.Fatalf("the reply before the tool: %q", got)
	}
	if !replies[0].ended {
		t.Fatal("the reply before the tool was not ended")
	}
	if replies[1].continueFrom != replies[0].id || replies[1].open {
		t.Fatalf("the reply after the tool: continue_from=%q (want %q) open=%v", replies[1].continueFrom, replies[0].id, replies[1].open)
	}
	if got := joined(replies[1]); got != "Ya está: tiene la cita el jueves a las cinco. ¿Algo más?" {
		t.Fatalf("the reply after the tool: %q", got)
	}
}

// A reply past TTS_POINTER_MAX_CHARS is ended at a sentence end and the next continues it; nothing is lost or repeated.
func TestPointer_LongReplySplitsAtASentenceEnd(t *testing.T) {
	old := pointerMaxChars
	pointerMaxChars = 90
	defer func() { pointerMaxChars = old }()
	sents := []string{
		"Le cuento cómo funciona el servicio de principio a fin. ",
		"Primero recibimos su solicitud por teléfono o por la web. ",
		"Después un técnico revisa el caso y le llama en un día. ",
		"Si hace falta una visita, la programamos a su gusto. ",
		"Y al final le enviamos un resumen por correo.",
	}
	var chunks []string
	for _, s := range sents {
		for _, w := range strings.SplitAfter(s, " ") {
			if w != "" {
				chunks = append(chunks, w)
			}
		}
	}
	tts := newPointerTTS()
	ms := runPointerTurn(t, tts, streamed(3*time.Millisecond, chunks...), nil)
	defer ms.Close()
	replies, plain := tts.snapshot()
	if len(plain) != 0 || len(replies) < 3 {
		t.Fatalf("want at least three replies, got %d (plain %q)", len(replies), plain)
	}
	var all strings.Builder
	for i, r := range replies {
		txt := joined(r)
		all.WriteString(txt)
		if i > 0 && r.continueFrom != replies[i-1].id {
			t.Fatalf("reply %d does not continue the one before it", i)
		}
		if !r.ended {
			t.Fatalf("reply %d was not ended", i)
		}
		if i < len(replies)-1 && !strings.HasSuffix(strings.TrimSpace(txt), ".") {
			t.Fatalf("reply %d does not end at a sentence end: %q", i, txt)
		}
		if n := runes(strings.TrimSpace(txt)); n > 2*pointerMaxChars {
			t.Fatalf("reply %d is %d characters", i, n)
		}
	}
	if all.String() != strings.Join(sents, "") {
		t.Fatalf("the replies together:\n got %q\nwant %q", all.String(), strings.Join(sents, ""))
	}
}

// Spoken truth from the pool's word marks: what the caller heard is exactly the words whose audio had played, never
// the text appended ahead of its audio.
func TestPointer_HeardWordsComeFromTheWordMarks(t *testing.T) {
	ms := &ManagedStream{playbackRate: 16000}
	gen := 2
	marks := &wordMarks{}
	t0 := time.Unix(1000, 0)
	ms.mu.Lock()
	ms.beginSegmentLocked(gen, "Claro, te lo miro")
	ms.curSeg.marks = marks
	ms.extendSegmentLocked(gen, "ahora mismo en el sistema.") // appended, not yet said
	n := 2 * 16000 / 10                                       // 100 ms
	for i := 0; i < 6; i++ {
		ms.scheduleAudioLocked(gen, n, t0)
	}
	for i, words := range []int{1, 2, 3, 4, 5, 6} {
		marks.marks = append(marks.marks, wordMark{at: time.Duration(i+1) * 100 * time.Millisecond, words: words})
	}
	text, _, complete := ms.playout.heard(t0.Add(350 * time.Millisecond))
	if text != "Claro, te lo" || complete {
		t.Fatalf("at 350 ms: %q complete=%v", text, complete)
	}
	// all its audio played, but the reply has not ended: only the words marked, not the text appended ahead of them
	text, _, complete = ms.playout.heard(t0.Add(2 * time.Second))
	if text != "Claro, te lo miro ahora mismo" || complete {
		t.Fatalf("at the end of the audio so far: %q complete=%v", text, complete)
	}
	if got := ms.playout.segs[0].heardWords(t0.Add(250 * time.Millisecond)); got != 2 {
		t.Fatalf("heardWords at 250 ms = %d", got)
	}
	marks.done = true
	for i := 0; i < 4; i++ {
		ms.scheduleAudioLocked(gen, n, t0)
	}
	text, _, complete = ms.playout.heard(t0.Add(2 * time.Second))
	ms.mu.Unlock()
	if text != "Claro, te lo miro ahora mismo en el sistema." || !complete {
		t.Fatalf("ended and played: %q complete=%v", text, complete)
	}
}

// A barge-in cancels the reply: nothing more is appended after it, and the context keeps only the words heard.
func TestPointer_BargeInKeepsTheWordsHeard(t *testing.T) {
	tts := newPointerTTS()
	tts.wordDelay = 60 * time.Millisecond
	llm := &chunkedLLM{script: [][]string{{"Claro, te lo miro ahora mismo en el sistema. ", "Tengo un hueco mañana por la tarde. ",
		"Y otro el jueves por la mañana."}}}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("pointer-barge"))
	defer ms.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(ctx, "hola"); close(done) }()
	select {
	case <-tts.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(200 * time.Millisecond)
	ms.mu.Lock()
	text, _, _ := ms.playout.heard(time.Now())
	ms.mu.Unlock()
	ms.handleInterrupt()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not end after the barge-in")
	}
	replies, _ := tts.snapshot()
	full := joined(replies[0])
	if text == "" || !strings.HasPrefix(full, text) || text == strings.TrimSpace(full) {
		t.Fatalf("heard %q of %q", text, full)
	}
	if n := len(strings.Fields(text)); n > replies[0].said {
		t.Fatalf("heard %d words but the pool had said %d", n, replies[0].said)
	}
}

// A reply the pool cannot finish is logged with its reason and reported; the rest is said again from the first word
// not finished, in a reply that continues it. A second failure speaks the rest sentence by sentence.
func TestPointer_AFailedReplyIsReportedAndTheRestReopened(t *testing.T) {
	for _, twice := range []bool{false, true} {
		tts := newPointerTTS()
		tts.failAfter[1] = 4
		if twice {
			tts.failAfter[2] = 2
		}
		tts.failReason = "watchdog"
		log := &recordingLogger{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, &chunkedLLM{script: [][]string{{pointerReply}}},
			tts, nil, DefaultConfig(), log)
		ms := orch.NewManagedStream(context.Background(), NewConversationSession("pointer-fail"))
		var errs []string
		var evMu sync.Mutex
		go func() {
			for ev := range ms.Events() {
				if ev.Type == ErrorEvent {
					evMu.Lock()
					errs = append(errs, fmt.Sprint(ev.Data))
					evMu.Unlock()
				}
			}
		}()
		ms.runLLMAndTTS(context.Background(), "hola")
		replies, plain := tts.snapshot()
		if twice {
			if len(replies) != 2 || len(plain) == 0 {
				t.Fatalf("twice: want two replies then sentences, got %d replies, plain %q", len(replies), plain)
			}
			rest := strings.Join(plain, " ")
			if !strings.HasSuffix(rest, "si le va bien.") {
				t.Fatalf("twice: the sentences after the second failure: %q", plain)
			}
			if log.count("WARN Pointer reply failed again") != 1 {
				t.Fatalf("no evidence of the fallback: %v", log.lines)
			}
		} else {
			if len(replies) != 2 || len(plain) != 0 {
				t.Fatalf("want the failed reply and one that continues it, got %d replies, plain %q", len(replies), plain)
			}
			words := strings.Fields(pointerReply)
			if got := strings.TrimSpace(joined(replies[1])); got != strings.Join(words[4:], " ") {
				t.Fatalf("the reply after the failure opens at the fifth word: %q", got)
			}
			if replies[1].continueFrom != replies[0].id {
				t.Fatal("the reply after the failure does not continue the failed one")
			}
		}
		if n := log.count("WARN Pointer reply "); n < 1 {
			t.Fatalf("the failed reply left no log line: %v", log.lines)
		}
		ms.Close()
		time.Sleep(10 * time.Millisecond)
		evMu.Lock()
		reported := strings.Join(errs, "|")
		evMu.Unlock()
		if !strings.Contains(reported, "TTS error") {
			t.Fatalf("no error event for the failed reply: %q", reported)
		}
	}
}

// A pool that does not answer as a word-pointer pool gets the rest of the reply sentence by sentence, as today.
func TestPointer_APoolThatRefusesGetsTheRestAsSentences(t *testing.T) {
	tts := newPointerTTS()
	tts.pointer = false
	log := &recordingLogger{}
	ms := runPointerTurn(t, tts, streamed(20*time.Millisecond, "Claro, te lo miro ahora mismo en el sistema. ",
		"Tengo un hueco mañana por la tarde. ", "Y otro el jueves."), log)
	defer ms.Close()
	replies, plain := tts.snapshot()
	if len(replies) != 1 {
		t.Fatalf("want one refused opening, got %d", len(replies))
	}
	all := strings.TrimSpace(replies[0].opening) + " " + strings.Join(plain, " ")
	if all != "Claro, te lo miro ahora mismo en el sistema. Tengo un hueco mañana por la tarde. Y otro el jueves." {
		t.Fatalf("opening %q then sentences %q", replies[0].opening, plain)
	}
	if log.count("INFO Pointer reply: the pool does not speak it as one stream") != 1 {
		t.Fatalf("the fallback left no evidence: %v", log.lines)
	}
}

// A refused append (no room) ends the reply after what it has; the refused text opens the next reply, which continues.
func TestPointer_AnAppendRefusedForRoomOpensTheNextReply(t *testing.T) {
	tts := newPointerTTS()
	tts.refuseRoom[[2]int{1, 2}] = true
	chunks := []string{"Claro, te lo miro ahora mismo en el sistema. ", "Tengo un hueco mañana por la tarde ", "y otro el ",
		"jueves por la mañana, ", "si le va bien."}
	ms := runPointerTurn(t, tts, streamed(25*time.Millisecond, chunks...), nil)
	defer ms.Close()
	replies, plain := tts.snapshot()
	if len(replies) != 2 || len(plain) != 0 {
		t.Fatalf("want two replies, got %d (plain %q)", len(replies), plain)
	}
	if got := joined(replies[0]) + joined(replies[1]); got != strings.Join(chunks, "") {
		t.Fatalf("the two replies together: %q", got)
	}
	if replies[1].continueFrom != replies[0].id {
		t.Fatal("the next reply does not continue the full one")
	}
}

// A tool call written out as speech stops the try before any of it reaches the voice; the next try's words open a
// reply that continues what was said.
func TestPointer_UnspeakableTextNeverReachesTheVoice(t *testing.T) {
	tts := newPointerTTS()
	llm := &chunkedLLM{script: [][]string{
		{"Vale, te lo apunto ahora mismo en la agenda. ", "Llamo a ", `{"name": "book"}`},
		{"Listo, ya está apuntado."},
	}}
	ms := runPointerTurn(t, tts, llm, nil)
	defer ms.Close()
	replies, _ := tts.snapshot()
	var all []string
	for _, r := range replies {
		all = append(all, joined(r))
	}
	got := strings.Join(all, "|")
	if strings.Contains(got, `{"`) || strings.Contains(got, "name") {
		t.Fatalf("the written-out call reached the voice: %q", got)
	}
	if !strings.Contains(got, "Listo, ya está apuntado.") || !strings.HasPrefix(got, "Vale, te lo apunto") {
		t.Fatalf("replies: %q", got)
	}
}

// Off (the provider does not report a word-pointer pool): today's path, one synthesis per sentence, no PointerReply.
func TestPointer_OffIsTodaysPath(t *testing.T) {
	tts := newPointerTTS()
	tts.enabled = false
	ms := runPointerTurn(t, tts, &chunkedLLM{script: [][]string{{"Claro, te lo miro ahora mismo. ", "Tengo un hueco mañana. ", "Y otro el jueves."}}}, nil)
	defer ms.Close()
	replies, plain := tts.snapshot()
	if len(replies) != 0 || len(plain) != 3 {
		t.Fatalf("want three plain sentences and no pointer reply, got %d replies, plain %q", len(replies), plain)
	}
}

// A written word the pool had half said ("twenty" of "25") when it failed is neither heard nor skipped: the next reply
// says it again from its start.
func TestPointer_AHalfSaidWordIsSaidAgainWhole(t *testing.T) {
	tts := newPointerTTS()
	text := "Claro, te lo miro: a las 10:30 de la mañana."
	tts.halfFail = 6 // "10:30"
	ms := runPointerTurn(t, tts, &chunkedLLM{script: [][]string{{text}}}, nil)
	defer ms.Close()
	replies, _ := tts.snapshot()
	if len(replies) != 2 {
		t.Fatalf("want the failed reply and its continuation, got %d", len(replies))
	}
	if got := strings.TrimSpace(joined(replies[1])); got != "10:30 de la mañana." {
		t.Fatalf("the continuation opens with %q", got)
	}
}
