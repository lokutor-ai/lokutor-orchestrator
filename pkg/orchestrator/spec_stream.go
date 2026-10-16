package orchestrator

import (
	"context"
	"os"
	"strings"
	"sync"
)

// Streamed speculation (2026-10-16).
//
// The speculative run asked with Complete, so a guess could only be used once the whole reply had arrived: the turn,
// confirmed, sat waiting for it (gate_spec_await_ms, median 234 ms over 719 production turns, 7 days to 2026-10-16),
// and when the reply took longer than the 350 ms cap it was thrown away and the same question asked again from scratch
// (19% of turns: first audio 954 ms median, 2,011 ms p90, against 498 / 725 ms when the guess was used). Now the run
// streams into a specStream, and a turn whose transcript is the run's takes the stream over as its first round
// (streamedRound): whatever has arrived is spoken at once, the rest as it comes, and the question is never asked twice.
// It is the same request to the same model through the provider's own streaming path, which also cuts a reply that runs
// past its turn and hedges a slow first token, as every other turn's first round does. SPECULATIVE_STREAM=0 goes back
// to Complete.

// speculativeStreamEnabled reports whether speculative runs stream (the default).
func speculativeStreamEnabled() bool {
	return strings.TrimSpace(os.Getenv("SPECULATIVE_STREAM")) != "0"
}

// specItem is one thing a streamed reply delivered, in order: a piece of text or a tool call.
type specItem struct {
	chunk string
	call  *ToolCallEventData
}

// specStream holds what a speculative run's stream has delivered so far, for whoever takes it over.
type specStream struct {
	mu    sync.Mutex
	items []specItem
	done  bool
	err   error
	wake  chan struct{} // closed, and replaced, whenever something new arrives or the stream ends
}

func newSpecStream() *specStream { return &specStream{wake: make(chan struct{})} }

func (s *specStream) push(it specItem) {
	s.mu.Lock()
	s.items = append(s.items, it)
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

func (s *specStream) end(err error) {
	s.mu.Lock()
	s.done, s.err = true, err
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// from returns what arrived from index i on, whether the stream has ended (and how), and a channel that closes when
// there is more.
func (s *specStream) from(i int) ([]specItem, bool, error, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []specItem
	if i < len(s.items) {
		items = append(items, s.items[i:]...)
	}
	return items, s.done, s.err, s.wake
}

// specStreamRound is what trySpeculativeResponse hands runLLMAndTTS: a speculative stream for transcript.
type specStreamRound struct {
	transcript string
	stream     *specStream
	cancel     context.CancelFunc // stops the run's request
	tokens     *TokenUsage
}

// streamedRound answers a turn's first StreamComplete from a speculative run's stream, live: what has arrived is
// delivered at once, the rest as it comes. Every later request goes to the real provider (the rounds after tools, a
// retry). A stream that failed before delivering anything is replaced by a request to the real provider, so a failed
// guess costs the turn what a miss costs, nothing worse.
type streamedRound struct {
	real   StreamingLLMProvider
	round  *specStreamRound
	mu     sync.Mutex
	used   bool
	logged bool
}

func (r *streamedRound) Name() string { return r.real.Name() }

func (r *streamedRound) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return r.real.Complete(ctx, messages, tools)
}

func (r *streamedRound) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	r.mu.Lock()
	first := !r.used
	r.used = true
	r.mu.Unlock()
	if !first {
		return r.real.StreamComplete(ctx, messages, tools, onChunk, onToolCall)
	}
	var text strings.Builder
	delivered := 0
	for {
		items, done, streamErr, wake := r.round.stream.from(delivered)
		for _, it := range items {
			delivered++
			var err error
			if it.call != nil {
				if onToolCall != nil {
					err = onToolCall(*it.call)
				}
			} else {
				text.WriteString(it.chunk)
				if onChunk != nil {
					err = onChunk(it.chunk)
				}
			}
			if err != nil {
				// The turn stopped reading (a reply that is not speech, a barge-in): the rest is not wanted.
				r.round.cancel()
				return text.String(), err
			}
		}
		if done {
			if streamErr != nil && delivered == 0 {
				return r.real.StreamComplete(ctx, messages, tools, onChunk, onToolCall)
			}
			if p, c, t, ok := r.round.tokens.Snapshot(); ok {
				sink := TokenUsageFrom(ctx)
				sink.Record(p, c, t)
				sink.SetAnsweredBy(r.round.tokens.AnsweredBy())
			}
			return text.String(), streamErr
		}
		select {
		case <-wake:
		case <-ctx.Done():
			r.round.cancel()
			return text.String(), ctx.Err()
		}
	}
}

// TakeStream hands over the current run's stream if the run is guessing on finalTranscript and streams: the caller
// owns the request from here (Cancel no longer stops it; the round's cancel does), and the executor is free for the
// next utterance. ok is false when there is no such run; nothing changes then.
func (se *SpeculativeExecutor) TakeStream(finalTranscript string) (*specStreamRound, bool) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.state == SpecIdle || se.stream == nil || !sameUtterance(se.runTranscript, finalTranscript) {
		return nil, false
	}
	round := &specStreamRound{transcript: finalTranscript, stream: se.stream, cancel: se.cancel, tokens: se.streamTokens}
	if round.cancel == nil {
		round.cancel = func() {}
	}
	se.taken = se.runID
	se.cancel = nil
	se.stream = nil
	se.streamTokens = nil
	se.state = SpecIdle
	se.result = nil
	se.runTranscript = ""
	return round, true
}
