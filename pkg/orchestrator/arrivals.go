package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Mid-sentence arrivals (off unless the provider says so, SentenceArriver.ArrivalsEnabled).
//
// With reply sessions, each sentence of a reply is its own synthesis request, and the pool appends it to the reply's
// stream only after the previous sentence has stopped: a stop and a restart at every join. Those restarts are where
// the voice model repeats or stalls (8.3% of sentences after a short opener, 2.1% after a normal one, 2026-11-23). With
// arrivals, each later sentence is handed to the stream that is still speaking the reply, the moment the LLM writes it,
// and its audio comes out of that same open response: no stop, no restart. A sentence the stream cannot take is spoken
// as today (its own request), and every sentence after it waits for that request's stream, so the order never changes.

// SentenceArriver is an optional TTSProvider capability: the reply's running synthesis stream (the one opened by the
// StreamSynthesize call in flight for ctx's reply, WithReply) can take the reply's next sentence while it speaks.
// Arrive returns true when the sentence was accepted: its audio will come out of that open stream, after what is
// already in it. false means it was not (no stream speaking, the stream already stopped, the pool refused it, an
// error): the caller speaks it as its own synthesis, as without arrivals.
type SentenceArriver interface {
	ArrivalsEnabled() bool
	Arrive(ctx context.Context, text string, voice Voice, lang Language) bool
}

// arrivalSecondsPerChar is the pace used to place a stream's audio on its sentences (spoken truth): one stream carries
// several sentences and nothing in the audio marks where one ends, so sentence k is taken to end once the stream has
// played (characters of sentences 0..k) x this. Production speech runs ~15 characters a second. An ESTIMATE: a
// sentence's own boundary can be off by its length times the pace error; the last known sentence takes whatever audio
// remains, and once the stream ends any sentence never reached is added to the last one (nothing arrived goes
// unrecorded).
var arrivalSecondsPerChar = 1.0 / 15.0

// sentenceSource is one reply's sentences in order: fed from the LLM's queue, taken by the speaker loop and the
// arrival feeder, and given back (at the front) when a stream refuses one.
type sentenceSource struct {
	mu      sync.Mutex
	items   []string
	closed  bool
	changed chan struct{}
}

func newSentenceSource(in <-chan string) *sentenceSource {
	s := &sentenceSource{changed: make(chan struct{})}
	go func() {
		for t := range in {
			s.mu.Lock()
			s.items = append(s.items, t)
			s.signalLocked()
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.closed = true
		s.signalLocked()
		s.mu.Unlock()
	}()
	return s
}

func (s *sentenceSource) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// pushFront gives a sentence back: it is the next one taken.
func (s *sentenceSource) pushFront(t string) {
	s.mu.Lock()
	s.items = append([]string{t}, s.items...)
	s.signalLocked()
	s.mu.Unlock()
}

// pop takes the next sentence, waiting for one. ok is false once the source is closed and empty, or when stop is done
// first (a nil stop never is).
func (s *sentenceSource) pop(stop <-chan struct{}) (string, bool) {
	for {
		s.mu.Lock()
		if len(s.items) > 0 {
			t := s.items[0]
			s.items = s.items[1:]
			s.mu.Unlock()
			return t, true
		}
		if s.closed {
			s.mu.Unlock()
			return "", false
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-stop:
			return "", false
		}
	}
}

// joinShortOpenerFrom is joinShortOpener for a reply read from a sentenceSource: the same rule, the same wait.
func joinShortOpenerFrom(ctx context.Context, first string, src *sentenceSource, wait time.Duration) (string, bool) {
	if !isShortOpener(first) {
		return first, false
	}
	stop := make(chan struct{})
	t := time.AfterFunc(wait, func() { close(stop) })
	defer t.Stop()
	done := make(chan struct{})
	defer close(done)
	stopOrCtx := make(chan struct{})
	go func() {
		select {
		case <-stop:
		case <-ctx.Done():
		case <-done:
			return
		}
		close(stopOrCtx)
	}()
	next, ok := src.pop(stopOrCtx)
	if !ok || strings.TrimSpace(next) == "" {
		return first, false
	}
	return strings.TrimSpace(first) + " " + strings.TrimSpace(next), true
}

// speakReplyWithArrivals is the reply loop of runStreamingLLM when the provider takes arrivals: each speakText call
// opens one stream, and the sentences the LLM writes while it speaks are handed to it (arrivalFeed).
func (ms *ManagedStream) speakReplyWithArrivals(ctx, rctx context.Context, arr SentenceArriver, queue <-chan string, gen int) {
	src := newSentenceSource(queue)
	first := true
	for {
		text, ok := src.pop(nil)
		if !ok {
			return
		}
		if first {
			// The short-opener merge stays ON with arrivals. A lone "Vale." is synthesised in ~100-200 ms, before the
			// LLM's next sentence lands (production: 172/374 ms p50/p90 after the first), so that sentence would arrive
			// after the opener's stream stopped and take the same stop-and-restart (ar_append) the merge exists to avoid,
			// the join after which 8.3% of sentences repeat or stall.
			first = false
			if joined, merged := joinShortOpenerFrom(ctx, text, src, shortOpenerWait); merged {
				ms.logger.Info("Short opener spoken with the next sentence", "gen", gen, "opener_len", len(text), "text_len", len(joined))
				text = joined
			}
		}
		ms.speakTextWith(rctx, text, gen, &arrivalFeed{arr: arr, src: src, texts: []string{text}}, nil)
	}
}

// arrivalFeed is one stream's arrivals: the sentences it carries (the one it was opened with, then each one that
// arrived), the feeder that hands it the next ones, and where its audio is on them (spoken truth).
type arrivalFeed struct {
	arr SentenceArriver
	src *sentenceSource

	mu    sync.Mutex
	texts []string
	chars []int // runes of texts[i], filled lazily

	// attribution (ms.mu held by the caller)
	audioBytes int64
	cur        int

	started bool
	stop    chan struct{}
	done    chan struct{}
}

func (f *arrivalFeed) add(t string) {
	f.mu.Lock()
	f.texts = append(f.texts, t)
	f.mu.Unlock()
}

// arrived is the number of sentences that joined the stream after the one it opened with.
func (f *arrivalFeed) arrived() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.texts) - 1
}

func (f *arrivalFeed) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.texts, " ")
}

// start runs the feeder once the stream has delivered its first audio (the provider takes arrivals only from then on).
func (f *arrivalFeed) start(ms *ManagedStream, ctx context.Context, gen int, voice Voice, lang Language) {
	if f.started {
		return
	}
	f.started = true
	f.stop = make(chan struct{})
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		for {
			if ctx.Err() != nil {
				return
			}
			t, ok := f.src.pop(f.stop)
			if !ok {
				return
			}
			if ctx.Err() != nil {
				f.src.pushFront(t)
				return
			}
			if ms.session != nil {
				if reason := unspeakableReply(t, ms.session.GetTools()); reason != "" {
					// what speakText would do with it: not spoken, and said so
					ms.logger.Warn("Not speaking: the reply text is not speech", "reason", reason, "gen", gen, "text", ContentForLog(t))
					continue
				}
			}
			ms.mu.Lock()
			talking := ms.vadSpeaking
			ms.mu.Unlock()
			if talking {
				// The caller is speaking over the reply: this sentence goes through speakText's own check (discarded
				// there if they still are), as it would without arrivals.
				f.src.pushFront(t)
				return
			}
			began := time.Now()
			// No character is counted here: the agent path is billed by time and tokens, not characters.
			if !f.arr.Arrive(ctx, t, voice, lang) {
				f.src.pushFront(t)
				ms.logger.Info("Sentence not taken by the running stream: spoken as its own synthesis", "gen", gen,
					"chars", utf8.RuneCountInString(t), "ms", time.Since(began).Milliseconds())
				return
			}
			f.add(t)
			ms.logger.Info("Sentence arrived in the running stream", "gen", gen, "chars", utf8.RuneCountInString(t),
				"arrivals", f.arrived(), "ms", time.Since(began).Milliseconds())
		}
	}()
}

// finish stops the feeder at the stream's end and waits for an Arrive in flight to decide (unless the turn was
// cancelled: then nothing it decides is spoken anyway).
func (f *arrivalFeed) finish(cancelled bool) {
	if !f.started {
		return
	}
	close(f.stop)
	if !cancelled {
		<-f.done
	}
}

// attributeLocked moves the current spoken segment to the next sentence once the stream's audio has passed the
// estimated end of the current one (arrivalSecondsPerChar). ms.mu held.
func (f *arrivalFeed) attributeLocked(ms *ManagedStream, gen, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audioBytes += int64(n)
	if ms.playbackRate <= 0 {
		return
	}
	played := float64(f.audioBytes) / float64(2*ms.playbackRate)
	for f.cur+1 < len(f.texts) {
		for len(f.chars) < len(f.texts) {
			f.chars = append(f.chars, utf8.RuneCountInString(f.texts[len(f.chars)]))
		}
		through := 0
		for i := 0; i <= f.cur; i++ {
			through += f.chars[i]
		}
		if played < float64(through)*arrivalSecondsPerChar {
			return
		}
		f.cur++
		ms.beginSegmentLocked(gen, f.texts[f.cur])
		ms.lastResponseText = f.texts[f.cur]
	}
}

// settleLocked, once the stream has ended whole, puts every arrived sentence its audio never reached (the estimate ran
// long) into the last segment, so the record of what was played has every sentence. ms.mu held.
func (f *arrivalFeed) settleLocked(ms *ManagedStream, gen int) {
	f.mu.Lock()
	rest := append([]string(nil), f.texts[min(f.cur+1, len(f.texts)):]...)
	f.cur = len(f.texts) - 1
	f.mu.Unlock()
	if len(rest) == 0 {
		return
	}
	ms.extendSegmentLocked(gen, strings.Join(rest, " "))
}

// extendSegmentLocked adds text to the current spoken segment of gen, on the playout timeline too (ms.mu held).
func (ms *ManagedStream) extendSegmentLocked(gen int, extra string) {
	extra = strings.TrimSpace(extra)
	if extra == "" || ms.curSeg.gen != gen {
		return
	}
	ms.curSeg.text = strings.TrimSpace(ms.curSeg.text + " " + extra)
	if ms.playout.gen == gen {
		for i := range ms.playout.segs {
			if ms.playout.segs[i].id == ms.curSeg.id {
				ms.playout.segs[i].text = ms.curSeg.text
			}
		}
	}
	if ms.echoReplyGen == gen {
		ms.echoReplyText = strings.TrimSpace(ms.echoReplyText + " " + extra)
	}
	ms.lastResponseText = ms.curSeg.text
}
