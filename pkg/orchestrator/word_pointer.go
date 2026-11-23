package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Word-pointer synthesis (off unless the TTS provider says so: WordPointerTTS.WordPointerEnabled, which the worker
// gives only when its switch is on AND the synthesis pool has answered that it serves a word-pointer model).
//
// A word-pointer voice model speaks a whole reply as ONE stream that follows the text word by word: text is appended
// while it speaks, and the reply ends only when the model moves past an end marker. There is no stop and restart
// between sentences, so none of the repeats, stalls and cuts that the joins between sentence requests produce. The
// pool's side is lokutor_tts cmd/versa_server (docs/design/word-pointer-serving.md); this file is the worker's:
//
//   - Feeding words. A reply opens with the text known when its first audio should start: its first sentence, or
//     TTS_POINTER_OPEN_WORDS (6) whole words, never fewer than that unless the reply is shorter (a short opener such
//     as "Vale." waits for more, as today). After that every piece the LLM writes is appended as it streams, in whole
//     words and ending at a word boundary with its punctuation and space, holding back the word still being written
//     and any tail that could still change when the next words arrive (pointerSafeEnd: a number, decimal or time, an
//     abbreviation, a symbol, a bracket tag, the start of a multi-word dictionary term) plus TTS_POINTER_GUARD_WORDS
//     (2) words so the check for a tool call or reasoning written as speech (unspeakableReply) still sees them first.
//     When the LLM's stream ends, the rest goes with the end.
//   - Closing and reopening. At a tool call the reply ends after the words said before it; the reply after the tool
//     is a new one that continues the previous reply's last seconds (continue_from on the pool). A reply past
//     TTS_POINTER_MAX_CHARS (600) characters is ended at a sentence end and the next continues it, and so is one whose
//     cache room runs low or whose append was refused for room.
//   - Spoken truth. The pool reports each word it finishes with its time in the reply's audio and where it ends in
//     the reply's text; a barge-in records exactly the words whose audio had played (playoutTimeline.heard), not the
//     per-sentence estimate.
//   - Failures. A reply the pool could not finish (capacity, its dwell watchdog, a text source that went quiet) is
//     logged with its reason and reported (the TTS error event), and the rest of the reply is said again from the
//     first word the caller did not hear, in a new reply that continues it; a second failure in the same turn speaks
//     the rest sentence by sentence, as without the pointer.
//   - Fallback. A pool that does not answer as a word-pointer pool (no X-Versa-Pointer, or an error before any
//     audio) gets the rest of the reply sentence by sentence, as today, logged.
//
// Billing is unchanged: the reply's text is what the LLM wrote, recorded as today; the agent path bills time and
// tokens, never synthesis characters.

var (
	pointerOpenWords  = envIntDefault("TTS_POINTER_OPEN_WORDS", 6)
	pointerOpenWait   = time.Duration(envIntDefault("TTS_POINTER_OPEN_WAIT_MS", 1500)) * time.Millisecond
	pointerMaxChars   = envIntDefault("TTS_POINTER_MAX_CHARS", 600)
	pointerGuardWords = envIntDefault("TTS_POINTER_GUARD_WORDS", 2)
	// pointerRoomMin: below this many cache positions left, the reply is ended at its next sentence end.
	pointerRoomMin = envIntDefault("TTS_POINTER_ROOM_MIN", 96)
	// pointerContinueTTL: a reply continues the one before it only when that one ended this recently (the pool keeps
	// a reply's last seconds for 30 s).
	pointerContinueTTL = 20 * time.Second
	// pointerFailuresBeforeSentences: failed replies (other than a stalled text source) a turn may reopen before its
	// rest is spoken sentence by sentence.
	pointerFailuresBeforeSentences = 1
)

// WordPointerTTS is an optional TTSProvider capability: the pool serves a word-pointer model and the worker's switch
// is on. Read once per reply.
type WordPointerTTS interface {
	WordPointerEnabled() bool
}

// pointerTermer is an optional TTSProvider capability: the session's multi-word dictionary terms, whose words must be
// laid together (each piece of text is normalised and respelled on its own).
type pointerTermer interface {
	WordPointerTerms() []string
}

// PointerVerdict is the pool's answer to an append.
type PointerVerdict struct {
	Arrived bool
	Mode    string // "appended", "pending", "held"
	Reason  string // when not arrived: "no_stream", "cancelled", "ended", "empty", "capacity"
	Room    int    // cache positions the reply has left, -1 unknown
}

// PointerWord is one word the pool's model finished saying.
type PointerWord struct {
	Index   int
	StartMs float64 // from the reply's first sample
	EndMs   float64
	// TextEnd is where the word ends in the reply's own text (its opening text, then every appended text, in runes as
	// handed to the provider); -1 when the pool did not say.
	TextEnd int
	// TextStart is where the WRITTEN word this spoken word belongs to begins (-1 unknown). A written word that is said
	// as several ("25", "twenty five") has the same TextStart and TextEnd on each: a caption lights it from the first
	// one's StartMs to the last one's EndMs.
	TextStart int
	// WrittenDone: this is the last spoken word of its written word, which is now said in full. Spoken truth and the
	// point a failed reply is said again from count only written words said in full.
	WrittenDone bool
}

// PointerAppendFunc sends the reply's next text (end: no more after it) to the pod speaking it.
type PointerAppendFunc func(ctx context.Context, text string, end bool) (PointerVerdict, error)

// PointerReply is one word-pointer reply, carried in the context of the StreamSynthesize call that opens it
// (WithPointerReply). The provider sends ID, ContinueFrom and Open with the opening text, and reports back what the
// pool answered (Opened), each finished word (Word) and a failure the pool named (Failed).
type PointerReply struct {
	ID           string
	ContinueFrom string // a reply this one continues (its last seconds as the reference), "" for none
	Open         bool   // more text will be appended; false: the opening text is the whole reply

	mu        sync.Mutex
	opened    bool
	pointer   bool
	continued bool
	appendFn  PointerAppendFunc
	failure   string
	err       error
	words     int
	seen      []PointerWord // the words reported (the first 4096)
	lastEnd   int           // the furthest TextEnd reported
	onOpened  func(pointer bool)
	onWord    func(PointerWord)
}

type pointerReplyCtxKey struct{}

// WithPointerReply marks ctx's synthesis as the opening of a word-pointer reply.
func WithPointerReply(ctx context.Context, r *PointerReply) context.Context {
	return context.WithValue(ctx, pointerReplyCtxKey{}, r)
}

// PointerReplyFrom is the word-pointer reply ctx's synthesis opens, or nil.
func PointerReplyFrom(ctx context.Context) *PointerReply {
	r, _ := ctx.Value(pointerReplyCtxKey{}).(*PointerReply)
	return r
}

// Opened is the provider's report of the pool's answer: pointer is whether the pool speaks the reply as one
// word-pointer stream (X-Versa-Pointer: 1), continued whether it continued ContinueFrom, fn where its text goes.
func (r *PointerReply) Opened(pointer, continued bool, fn PointerAppendFunc) {
	r.mu.Lock()
	if r.opened {
		r.mu.Unlock()
		return
	}
	r.opened, r.pointer, r.continued, r.appendFn = true, pointer, continued, fn
	cb := r.onOpened
	r.mu.Unlock()
	if cb != nil {
		cb(pointer)
	}
}

// Word is the provider's report of a word the model finished.
func (r *PointerReply) Word(w PointerWord) {
	r.mu.Lock()
	r.words++
	if len(r.seen) < 4096 {
		r.seen = append(r.seen, w)
	}
	if w.WrittenDone && w.TextEnd > r.lastEnd {
		r.lastEnd = w.TextEnd
	}
	cb := r.onWord
	r.mu.Unlock()
	if cb != nil {
		cb(w)
	}
}

// Failed is the provider's report of the reason the pool gave for a reply it could not finish.
func (r *PointerReply) Failed(reason string) {
	r.mu.Lock()
	r.failure = reason
	r.mu.Unlock()
}

func (r *PointerReply) setErr(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

// PointerReplyState is what a reply's provider has reported so far.
type PointerReplyState struct {
	Opened, Pointer, Continued bool
	Failure                    string
	Words                      []PointerWord
}

// State is what the provider has reported for the reply so far.
func (r *PointerReply) State() PointerReplyState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return PointerReplyState{Opened: r.opened, Pointer: r.pointer, Continued: r.continued, Failure: r.failure,
		Words: append([]PointerWord(nil), r.seen...)}
}

// Append sends the reply's next text to the pod speaking it (through the provider's PointerAppendFunc).
func (r *PointerReply) Append(ctx context.Context, text string, end bool) (PointerVerdict, error) {
	return r.append(ctx, text, end)
}

func (r *PointerReply) append(ctx context.Context, text string, end bool) (PointerVerdict, error) {
	r.mu.Lock()
	fn := r.appendFn
	r.mu.Unlock()
	if fn == nil {
		return PointerVerdict{Reason: "no_stream", Room: -1}, nil
	}
	return fn(ctx, text, end)
}

// wordPointerOn reports whether this reply goes to a word-pointer pool.
func (ms *ManagedStream) wordPointerOn() bool {
	if ms.orch == nil || ms.orch.tts == nil {
		return false
	}
	wp, ok := ms.orch.tts.(WordPointerTTS)
	return ok && wp.WordPointerEnabled()
}

func (ms *ManagedStream) pointerTerms() []string {
	if t, ok := ms.orch.tts.(pointerTermer); ok {
		return t.WordPointerTerms()
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------- the text

// wordStream is the text the LLM has written for one turn's replies, as written, with the points where a reply must
// end (a tool call, a retry after an unspeakable sentence). Readers take text from it in order (take); a retry drops
// whatever no reply has taken yet.
type wordStream struct {
	mu       sync.Mutex
	text     string
	marks    []int
	closed   bool
	taken    int // the furthest offset any reply has taken
	tryStart int
	changed  chan struct{}
}

func newWordStream() *wordStream { return &wordStream{changed: make(chan struct{})} }

func (w *wordStream) signalLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *wordStream) write(chunk string) {
	if chunk == "" {
		return
	}
	w.mu.Lock()
	w.text += chunk
	w.signalLocked()
	w.mu.Unlock()
}

// mark ends the reply being spoken after everything written so far.
func (w *wordStream) mark() {
	w.mu.Lock()
	if n := len(w.marks); n == 0 || w.marks[n-1] != len(w.text) {
		w.marks = append(w.marks, len(w.text))
	}
	w.signalLocked()
	w.mu.Unlock()
}

// retry drops the sentence a stopped try was writing (what follows this try's last sentence end, unless a reply has
// already taken it), ends the reply there, and returns what this try keeps: it is said, or being said.
func (w *wordStream) retry() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := w.taken
	if ends := sentenceEndsIn(w.text[w.tryStart:], true); len(ends) > 0 && w.tryStart+ends[len(ends)-1] > keep {
		keep = w.tryStart + ends[len(ends)-1]
	}
	if keep < w.tryStart {
		keep = w.tryStart
	}
	said := strings.TrimSpace(w.text[w.tryStart:keep])
	w.text = w.text[:keep]
	kept := w.marks[:0]
	for _, m := range w.marks {
		if m <= len(w.text) {
			kept = append(kept, m)
		}
	}
	w.marks = kept
	if n := len(w.marks); n == 0 || w.marks[n-1] != len(w.text) {
		w.marks = append(w.marks, len(w.text))
	}
	if w.text != "" && !strings.HasSuffix(w.text, " ") {
		w.text += " "
	}
	w.tryStart = len(w.text)
	w.signalLocked()
	return said
}

func (w *wordStream) close() {
	w.mu.Lock()
	w.closed = true
	w.signalLocked()
	w.mu.Unlock()
}

// streamView is what a reader at offset `from` may consider (wordStream.view).
type streamView struct {
	text    string
	limit   int  // the end of the stretch from belongs to: the next mark after from, else the text's end
	marked  bool // a mark ends the stretch
	final   bool // nothing more will be written in it: marked, or the stream is closed
	closed  bool
	changed <-chan struct{} // closed at the next change
}

func (w *wordStream) view(from int) streamView {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := streamView{text: w.text, limit: len(w.text), closed: w.closed, changed: w.changed}
	for _, m := range w.marks {
		if m > from {
			v.limit, v.marked = m, true
			break
		}
	}
	if v.limit < from {
		v.limit = from
	}
	v.final = v.marked || w.closed
	return v
}

// take records that text up to `to` is given to a reply; false when a retry has dropped it meanwhile.
func (w *wordStream) take(to int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if to > len(w.text) {
		return false
	}
	if to > w.taken {
		w.taken = to
	}
	return true
}

// ---------------------------------------------------------------------------------------------------- whole words

type wordSpan struct{ start, end int } // byte offsets of a run of non-space characters

func wordSpans(s string) []wordSpan {
	var out []wordSpan
	start := -1
	for i, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				out = append(out, wordSpan{start, i})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, wordSpan{start, len(s)})
	}
	return out
}

// pointerSafeEnd is how much of s (text not yet given to the reply) may be appended now: whole words, ending after the
// whitespace that follows the last of them. Held back: the word still being written (no whitespace after it yet),
// then guard more words, then, walking back, every word whose reading can still change with what follows it
// (stickyWord, or the start of a multi-word term). final: nothing more will be written, all of it may go.
func pointerSafeEnd(s string, final bool, terms []string, guard int) int {
	if final {
		return len(s)
	}
	spans := wordSpans(s)
	n := len(spans)
	if n > 0 && spans[n-1].end == len(s) {
		n-- // still being written
	}
	n -= guard
	if n <= 0 {
		return 0
	}
	words := make([]string, n)
	for i := 0; i < n; i++ {
		words[i] = s[spans[i].start:spans[i].end]
	}
	n -= termPrefixHeld(words, terms)
	for n > 0 && stickyWord(words[n-1]) {
		n--
	}
	if n <= 0 {
		return 0
	}
	// through the whitespace after the last word released
	if n < len(spans) {
		return spans[n].start
	}
	return len(s)
}

// stickyWord reports whether a word's reading may still change with the word after it: it holds a digit ("3:30" before
// "p.m.", "25" before "%"), is a symbol on its own ("$", "€", "-"), a bracket tag ("[laugh]", whose cleaning drops it
// when nothing follows), or an abbreviation ("Sr.", "Dr.", "p.m.", "etc.": a dot inside it, or a dot after at most
// three letters).
func stickyWord(w string) bool {
	if strings.ContainsAny(w, "[]") {
		return true
	}
	letters, digits := 0, 0
	for _, r := range w {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if digits > 0 {
		return true
	}
	if letters == 0 {
		return !isOnlyPunctuation(w)
	}
	core := strings.TrimRight(w, ",;:)\"'»”’")
	if strings.HasSuffix(core, ".") && !strings.HasSuffix(core, "..") {
		inner := strings.TrimSuffix(core, ".")
		if strings.Contains(inner, ".") || utf8.RuneCountInString(strings.TrimLeft(inner, "(\"'«“‘¿¡")) <= 3 {
			return true
		}
	}
	return false
}

func isOnlyPunctuation(w string) bool {
	for _, r := range w {
		if !unicode.IsPunct(r) {
			return false
		}
	}
	return true
}

// termPrefixHeld is how many of words' last words are the start (not the whole) of a multi-word term.
func termPrefixHeld(words []string, terms []string) int {
	best := 0
	norm := func(s string) string {
		return strings.ToLower(strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) }))
	}
	for _, t := range terms {
		tw := strings.Fields(t)
		if len(tw) < 2 {
			continue
		}
		for p := len(tw) - 1; p >= 1 && p > best; p-- {
			if p > len(words) {
				continue
			}
			match := true
			for i := 0; i < p; i++ {
				if norm(words[len(words)-p+i]) != norm(tw[i]) {
					match = false
					break
				}
			}
			if match {
				best = p
				break
			}
		}
	}
	return best
}

// sentenceEndsIn are the byte offsets in s just after each sentence end (the terminator, then the whitespace after
// it), in order; final decides the ones that depend on text not written yet.
func sentenceEndsIn(s string, final bool) []int {
	var out []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			isEnd, decided := sentenceEndsAt(s, i, final)
			if !decided || !isEnd {
				continue
			}
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\n' || s[j] == '\t') {
				j++
			}
			out = append(out, j)
		}
	}
	return out
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// ---------------------------------------------------------------------------------------------------- one reply

// pointerFeed is one reply's appender: it takes the turn's text from start, opening with text[start:sent], and appends
// what follows as it is written, until a mark, the end of the stream, a split or a refusal ends the reply.
type pointerFeed struct {
	ms    *ManagedStream
	ws    *wordStream
	pr    *PointerReply
	gen   int
	start int
	terms []string

	mu       sync.Mutex
	sent     int    // the turn's text given to the reply so far (the opening, then each append)
	text     string // the reply's own text: what was handed to the provider, in order
	ended    bool   // the end was sent (or the reply opened closed)
	outcome  string // why the reply ended, for the log: "end", "mark", "split", "room", "capacity", "gone", "append_error"
	appends  int
	held     int // appends that left words behind (a tail that could still change, or the guard)
	waits    int // times the reply had said all it had and waited for the LLM
	marks    *wordMarks
	done     chan struct{}
	started  bool
	stopping chan struct{}
}

func (f *pointerFeed) replyText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

// run appends the turn's text to the reply until it ends. Called once the pool has said it speaks the reply as one
// stream (PointerReply.Opened with pointer true).
func (f *pointerFeed) run(ctx context.Context) {
	defer close(f.done)
	split := false // end the reply at its next sentence end (long, or its room running low)
	starved := false
	for {
		f.mu.Lock()
		sent, ended := f.sent, f.ended
		f.mu.Unlock()
		if ended || ctx.Err() != nil {
			return
		}
		vw := f.ws.view(sent)
		final, changed := vw.final, vw.changed
		avail := vw.text[sent:vw.limit]
		safe := pointerSafeEnd(avail, final, f.terms, pointerGuardWords)
		end := final && safe == len(avail)
		why := "end"
		if vw.marked {
			why = "mark"
		}
		f.mu.Lock()
		have := runes(f.text)
		f.mu.Unlock()
		if safe > 0 && (split || have+runes(avail[:safe]) > pointerMaxChars) {
			// a long reply (or one short of room) ends at a sentence end: the last one that keeps it under the cap,
			// else the first one there is; the rest opens the next reply, which continues this one
			ends := sentenceEndsIn(avail[:safe], final)
			cut := -1
			for _, e := range ends {
				if have+runes(avail[:e]) <= pointerMaxChars || cut < 0 {
					cut = e
				}
			}
			switch {
			case cut > 0 && (cut < safe || !end):
				safe, end, why = cut, true, "split"
				if split {
					why = "room"
				}
			case cut <= 0:
				split = true
			}
		}
		if safe == 0 && !end {
			if !starved && !final {
				// it has said (or been given) everything written; the pool waits for text
				starved = true
				f.mu.Lock()
				f.waits++
				f.mu.Unlock()
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return
			case <-f.stopping:
				return
			}
			continue
		}
		starved = false
		piece := avail[:safe]
		if !f.ws.take(sent + safe) {
			continue // a retry dropped it: look again
		}
		if safe < len(avail) && !end {
			f.mu.Lock()
			f.held++
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.text += piece
		f.mu.Unlock()
		v, err := f.pr.append(ctx, piece, end)
		if ctx.Err() != nil {
			return
		}
		if err != nil || (!v.Arrived && v.Reason != "empty") {
			f.mu.Lock()
			f.text = f.text[:len(f.text)-len(piece)]
			f.mu.Unlock()
			reason := v.Reason
			if err != nil {
				reason = "error: " + err.Error()
			}
			f.ms.logger.Warn("Pointer reply: text not taken by the pool", "gen", f.gen, "reply_id", f.pr.ID,
				"reason", reason, "chars", runes(piece), "end", end)
			switch {
			case v.Reason == "capacity" || err != nil:
				// close this reply after what it has; the piece opens the next one
				if _, e2 := f.pr.append(ctx, "", true); e2 != nil {
					f.ms.logger.Warn("Pointer reply: its end could not be sent", "gen", f.gen, "reply_id", f.pr.ID, "error", e2.Error())
				}
				outcome := "capacity"
				if err != nil {
					outcome = "append_error"
				}
				f.finish(sent, outcome)
			default:
				f.finish(sent, "gone") // the stream is over (failed or cancelled): speakPointerReply reads why
			}
			return
		}
		f.mu.Lock()
		f.sent = sent + safe
		f.appends++
		f.mu.Unlock()
		f.ms.extendPointerSegment(f.gen, piece)
		if v.Room >= 0 && v.Room < pointerRoomMin {
			split = true
		}
		if end {
			f.finish(sent+safe, why)
			return
		}
	}
}

func (f *pointerFeed) finish(sent int, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = sent
	f.ended = outcome != "gone"
	f.outcome = outcome
}

// extendPointerSegment adds an appended piece to the segment being spoken (the reply's text grows as it is appended;
// the words heard come from the pool's word marks, never from the text's length).
func (ms *ManagedStream) extendPointerSegment(gen int, piece string) {
	ms.mu.Lock()
	ms.extendSegmentLocked(gen, piece)
	ms.mu.Unlock()
}

// wordMarks is a word-pointer reply's finished words on its audio: each word's end (from the reply's first sample)
// and how many of the reply's written words it completes. Guarded by ms.mu.
type wordMarks struct {
	marks []wordMark
	// done: the reply ended normally, every word of its text spoken
	done bool
}

type wordMark struct {
	at    time.Duration
	words int
}

// heard is how many written words had been heard after h of the reply's audio.
func (m *wordMarks) heard(h time.Duration) int {
	n := 0
	for _, x := range m.marks {
		if x.at <= h+20*time.Millisecond && x.words > n {
			n = x.words
		}
	}
	return n
}

// ---------------------------------------------------------------------------------------------------- the turn

// pointerLast is the word-pointer reply the stream spoke last, which the next reply of the same turn continues.
type pointerLast struct {
	id  string
	at  time.Time
	gen int
}

type pointerContinueCtxKey struct{}

// withPointerContinue marks a reply as the continuation of generation gen's (the reply after gen's tool calls): its
// first word-pointer reply continues gen's last one.
func withPointerContinue(ctx context.Context, gen int) context.Context {
	return context.WithValue(ctx, pointerContinueCtxKey{}, gen)
}

// lastPointerFor is the reply a new reply continues: generation gen's last one, if it ended within pointerContinueTTL.
func (ms *ManagedStream) lastPointerFor(ctx context.Context) string {
	gen, ok := ctx.Value(pointerContinueCtxKey{}).(int)
	if !ok {
		return ""
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.lastPointer.id != "" && ms.lastPointer.gen == gen && time.Since(ms.lastPointer.at) < pointerContinueTTL {
		return ms.lastPointer.id
	}
	return ""
}

// speakPointerResponse speaks a reply known whole (the reply after a tool call, a speculative hit) as word-pointer
// replies: one, or several of at most pointerMaxChars cut at sentence ends, each continuing the one before; the reply
// after a tool call continues the words said before it (withPointerContinue).
func (ms *ManagedStream) speakPointerResponse(ctx context.Context, text string, gen int) {
	ws := newWordStream()
	ws.write(strings.TrimSpace(text))
	ws.close()
	ms.speakPointerReplies(ctx, ws, gen, ms.lastPointerFor(ctx), false)
}

// speakPointerReplies speaks the turn's text in ws as word-pointer replies, in order, until ws is closed and spoken,
// the turn is cancelled, or the pool turns out not to serve them (the rest then goes sentence by sentence).
// continueFrom: a reply the first one continues. firstOfTurn: the first reply may be a short opener that waits.
func (ms *ManagedStream) speakPointerReplies(ctx context.Context, ws *wordStream, gen int, continueFrom string, firstOfTurn bool) {
	offset := 0
	failures := 0
	terms := ms.pointerTerms()
	for seq := 1; ctx.Err() == nil; seq++ {
		openEnd, closed, ok := ms.pointerOpening(ctx, ws, offset, firstOfTurn, terms)
		if !ok {
			return
		}
		if strings.TrimSpace(ws.view(offset).text[offset:openEnd]) == "" {
			offset = openEnd
			continue
		}
		firstOfTurn = false
		pr := &PointerReply{ID: fmt.Sprintf("%s-%d", newReplyID(), seq), ContinueFrom: continueFrom, Open: !closed}
		res := ms.speakPointerReply(ctx, ws, pr, offset, openEnd, closed, gen, terms)
		if ctx.Err() != nil {
			return
		}
		switch {
		case res.refused:
			ms.logger.Info("Pointer reply: the pool does not speak it as one stream; the rest of the reply goes sentence by sentence",
				"gen", gen, "reply_id", pr.ID, "error", errString(res.err))
			ms.speakRestAsSentences(ctx, ws, res.resume, gen)
			return
		case res.failed:
			if res.failure != "text_stalled" || res.resume <= offset {
				failures++ // a stalled text source is reopened as often as it makes progress
			}
			if failures > pointerFailuresBeforeSentences {
				ms.logger.Warn("Pointer reply failed again: the rest of the reply goes sentence by sentence",
					"gen", gen, "reply_id", pr.ID, "reason", res.failure, "failures", failures)
				ms.speakRestAsSentences(ctx, ws, res.resume, gen)
				return
			}
			ms.logger.Info("Pointer reply: saying the rest again from the first word not heard, in a reply that continues it",
				"gen", gen, "reply_id", pr.ID, "reason", res.failure)
		}
		offset = res.resume
		continueFrom = pr.ID
		ms.mu.Lock()
		ms.lastPointer = pointerLast{id: pr.ID, at: time.Now(), gen: gen}
		ms.mu.Unlock()
	}
}

// pointerOpening waits until a reply can open at offset: the end of its opening text, whether that text is the whole
// reply (closed), and ok false when there is nothing left to say (the stream is closed and spoken) or ctx ended.
func (ms *ManagedStream) pointerOpening(ctx context.Context, ws *wordStream, offset int, firstOfTurn bool, terms []string) (end int, closed, ok bool) {
	began := time.Time{}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		vw := ws.view(offset)
		limit, final, changed := vw.limit, vw.final, vw.changed
		avail := vw.text[offset:limit]
		if strings.TrimSpace(avail) == "" {
			if final && limit > offset {
				return limit, true, true // a mark right here: nothing to say before it
			}
			if vw.closed && limit == len(vw.text) {
				return 0, false, false
			}
		}
		if final && strings.TrimSpace(avail) != "" {
			// all of this stretch is known: a reply closed with it, cut at a sentence end when it is long
			if runes(avail) <= pointerMaxChars {
				return limit, true, true
			}
			cut := -1
			for _, e := range sentenceEndsIn(avail, true) {
				if runes(avail[:e]) <= pointerMaxChars || cut < 0 {
					cut = e
				}
				if runes(avail[:e]) > pointerMaxChars {
					break
				}
			}
			if cut > 0 && cut < len(avail) {
				return offset + cut, true, true
			}
			return limit, true, true
		}
		safe := pointerSafeEnd(avail, false, terms, pointerGuardWords)
		if safe > 0 {
			if began.IsZero() {
				began = time.Now()
			}
			words := len(strings.Fields(avail[:safe]))
			ends := sentenceEndsIn(avail[:safe], false)
			short := firstOfTurn && len(ends) == 1 && isShortOpener(avail[:ends[0]])
			switch {
			case words >= pointerOpenWords:
				return offset + safe, false, true
			case len(ends) > 0 && !short:
				return offset + safe, false, true
			case len(ends) > 0 && short && time.Since(began) >= shortOpenerWait:
				return offset + safe, false, true
			case time.Since(began) >= pointerOpenWait:
				return offset + safe, false, true
			}
		}
		var wake <-chan time.Time
		if !began.IsZero() {
			wait := pointerOpenWait - time.Since(began)
			if firstOfTurn && shortOpenerWait-time.Since(began) < wait {
				wait = shortOpenerWait - time.Since(began)
			}
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(wait)
			wake = timer.C
		}
		select {
		case <-changed:
		case <-wake:
		case <-ctx.Done():
			return 0, false, false
		}
	}
}

type pointerResult struct {
	resume  int // where the next reply (or the sentences after a fallback) starts in the turn's text
	refused bool
	failed  bool
	failure string
	err     error
}

// speakPointerReply opens one reply with text[offset:openEnd] and feeds it until it ends.
func (ms *ManagedStream) speakPointerReply(ctx context.Context, ws *wordStream, pr *PointerReply, offset, openEnd int,
	closed bool, gen int, terms []string) pointerResult {
	opening := ws.view(offset).text[offset:openEnd]
	if !ws.take(openEnd) {
		return pointerResult{resume: offset}
	}
	f := &pointerFeed{ms: ms, ws: ws, pr: pr, gen: gen, start: offset, terms: terms, sent: openEnd, text: opening,
		ended: closed, marks: &wordMarks{}, done: make(chan struct{}), stopping: make(chan struct{})}
	if closed {
		f.outcome = "end"
	}
	fctx, fcancel := context.WithCancel(ctx)
	defer fcancel()
	pr.mu.Lock()
	pr.onOpened = func(pointer bool) {
		if pointer && pr.Open {
			f.mu.Lock()
			f.started = true
			f.mu.Unlock()
			go f.run(fctx)
		}
	}
	pr.onWord = func(w PointerWord) {
		if w.TextEnd < 0 || !w.WrittenDone {
			return // a written word half said ("twenty" of "25") is not claimed as heard
		}
		rt := f.replyText()
		rr := []rune(rt)
		te := w.TextEnd
		if te > len(rr) {
			te = len(rr)
		}
		n := len(strings.Fields(string(rr[:te])))
		ms.mu.Lock()
		f.marks.marks = append(f.marks.marks, wordMark{at: time.Duration(w.EndMs * float64(time.Millisecond)), words: n})
		ms.mu.Unlock()
	}
	pr.mu.Unlock()

	began := time.Now()
	ms.speakTextWith(WithPointerReply(ctx, pr), opening, gen, nil, f.marks)
	f.mu.Lock()
	started := f.started
	f.mu.Unlock()
	if started {
		close(f.stopping)
		if ctx.Err() == nil {
			<-f.done
		}
	}
	pr.mu.Lock()
	opened, pointer, continued, failure, err, words, lastEnd := pr.opened, pr.pointer, pr.continued, pr.failure, pr.err, pr.words, pr.lastEnd
	pr.mu.Unlock()
	f.mu.Lock()
	sent, ended, outcome, appends, held, waits, replyText := f.sent, f.ended, f.outcome, f.appends, f.held, f.waits, f.text
	f.mu.Unlock()

	res := pointerResult{resume: sent, err: err}
	cancelled := ctx.Err() != nil
	switch {
	case cancelled:
		outcome = "cancelled"
	case !opened || !pointer:
		// the pool never answered (an error before any audio) or answered as a sentence pool: the opening was spoken
		// (or not) as an ordinary synthesis; the rest goes sentence by sentence from where the opening ended (or from
		// its start when nothing was spoken)
		res.refused = true
		res.resume = openEnd
		if !opened {
			res.resume = offset
		}
		outcome = "refused"
	case err != nil || failure != "" || !ended:
		// the pool could not finish the reply: say the rest again from the first word the caller did not hear
		res.failed = true
		res.failure = failure
		if res.failure == "" {
			res.failure = "stream_error"
			if err == nil {
				res.failure = "ended_early"
			}
		}
		res.resume = offset + pointerResumeAt(replyText, lastEnd)
		outcome = "failed"
	}
	if !res.refused && !res.failed && !cancelled {
		ms.mu.Lock()
		f.marks.done = true
		ms.mu.Unlock()
	}
	level := ms.logger.Info
	if res.failed {
		level = ms.logger.Warn
	}
	level("Pointer reply", "gen", gen, "reply_id", pr.ID, "continued_from", pr.ContinueFrom, "continued", continued,
		"chars", runes(replyText), "words", words, "appends", appends, "held", held, "waits", waits,
		"ended_by", outcome, "failure", res.failure, "ms", time.Since(began).Milliseconds())
	if res.failed {
		// evidence of audio lost (CLAUDE.md rule 6): the provider's error is already a TTS error event; a reply the
		// pool ended without one is reported here
		if err == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("TTS pointer reply failed (%s)", res.failure))
		}
	}
	return res
}

// pointerResumeAt is the byte offset in a reply's own text of the first word after the one ending at rune textEnd (the
// last word the pool finished), or 0 when it finished none.
func pointerResumeAt(replyText string, textEnd int) int {
	if textEnd <= 0 {
		return 0
	}
	rr := []rune(replyText)
	if textEnd > len(rr) {
		textEnd = len(rr)
	}
	b := len(string(rr[:textEnd]))
	for b < len(replyText) {
		r, size := utf8.DecodeRuneInString(replyText[b:])
		if !unicode.IsSpace(r) {
			break
		}
		b += size
	}
	return b
}

// speakRestAsSentences is today's path for the rest of a turn's text from offset: sentence by sentence, as written.
func (ms *ManagedStream) speakRestAsSentences(ctx context.Context, ws *wordStream, offset int, gen int) {
	for ctx.Err() == nil {
		vw := ws.view(offset)
		text, limit, final, closed, changed := vw.text, vw.limit, vw.final, vw.closed, vw.changed
		for {
			buf := text[offset:limit]
			e := nextFlushPoint(buf, final, false, 0, minSpokenSegment)
			if e <= 0 {
				break
			}
			if !ws.take(offset + e) {
				break
			}
			if seg := strings.TrimSpace(buf[:e]); seg != "" {
				ms.speakText(ctx, seg, gen)
			}
			offset += e
		}
		if final {
			if rest := strings.TrimSpace(text[offset:limit]); rest != "" && ws.take(limit) {
				ms.speakText(ctx, rest, gen)
			}
			offset = limit
			if closed && limit == len(text) {
				return
			}
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}
