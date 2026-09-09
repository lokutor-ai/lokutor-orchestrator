package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"
)

type SpeculativeState int

const (
	SpecIdle SpeculativeState = iota
	SpecRunning
	SpecReady
)

// SpeculativeResult is what a completed speculative run produced: the STT
// transcript it ran on, and — when the LLM stage also completed — the
// generated response text. PartialTranscript alone (Response == "") means
// only the STT stage finished before something (a mismatch, an error, or
// SpeculativeLLM being off) stopped it short of generation.
type SpeculativeResult struct {
	PartialTranscript string
	Response          string
}

// SpeculativeExecutor runs STT, and optionally LLM completion, on a guess
// at the user's utterance *before* the real end-of-turn is confirmed —
// specifically, triggered by a short in-utterance pause (~100ms) rather
// than waiting for the VAD hangover (~448ms) that actually gates when
// audio gets cut and a turn is confirmed. The hangover is still the only
// thing that ever commits to anything; this only ever pre-computes a
// candidate answer that gets used IF the eventually-confirmed transcript
// turns out to match what was speculated on — see Await.
type SpeculativeExecutor struct {
	mu         sync.Mutex
	state      SpeculativeState
	interval   time.Duration
	lastSpecAt time.Time
	result     *SpeculativeResult
	cancel     context.CancelFunc
	done       chan struct{} // closed when the current run finishes (any outcome)

	onPartial func(transcript string)
}

func NewSpeculativeExecutor(intervalMs int) *SpeculativeExecutor {
	if intervalMs <= 0 {
		intervalMs = 400
	}
	return &SpeculativeExecutor{
		interval: time.Duration(intervalMs) * time.Millisecond,
	}
}

func (se *SpeculativeExecutor) SetOnPartial(cb func(transcript string)) {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.onPartial = cb
}

// ShouldSpeculate is the original continuous-speech trigger: re-speculate
// periodically once an utterance has run long enough that a fresher
// transcript is worth the redundant STT/LLM cost. Kept as-is for that use;
// ShouldSpeculateOnPause below is the new, much earlier trigger.
func (se *SpeculativeExecutor) ShouldSpeculate(speechDuration time.Duration, lastSpecAt time.Time) bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.state != SpecIdle {
		return false
	}
	// Only speculate after 1.5s of speech — short utterances run the full pipeline
	if speechDuration < 1500*time.Millisecond {
		return false
	}
	if !lastSpecAt.IsZero() && time.Since(lastSpecAt) < se.interval {
		return false
	}
	return true
}

// ShouldSpeculateOnPause is the fast trigger: fire on a short in-utterance
// pause with no minimum speech duration (a one-word "yes" deserves an
// early guess just as much as a long sentence does) and no state
// requirement beyond "not already running" — callers pair this with their
// own short (~100ms) quiet-detection, independent of and much faster than
// the real VAD's hangover.
func (se *SpeculativeExecutor) ShouldSpeculateOnPause(lastSpecAt time.Time) bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.state != SpecIdle {
		return false
	}
	if !lastSpecAt.IsZero() && time.Since(lastSpecAt) < se.interval {
		return false
	}
	return true
}

// Start begins speculative STT, and — if llm is non-nil — speculative LLM
// completion once STT produces a non-trivial transcript. contextSnapshot is
// a point-in-time copy of the real conversation history (session.GetContextCopy())
// with the speculative transcript appended as a synthetic trailing user
// turn; it is NEVER written back to the live session — only the real,
// confirmed path (processUtterance) ever mutates session state. That's
// what makes a wrong guess safe: at worst it's wasted compute, since
// nothing it produces is visible or committed unless Await later confirms
// the guess was right.
func (se *SpeculativeExecutor) Start(ctx context.Context, orch *Orchestrator, audio []byte, lang Language, historySnapshot []Message, tools []Tool) {
	se.mu.Lock()
	if se.state != SpecIdle {
		se.mu.Unlock()
		return
	}
	se.state = SpecRunning
	se.result = nil
	sCtx, sCancel := context.WithTimeout(ctx, 8*time.Second)
	se.cancel = sCancel
	done := make(chan struct{})
	se.done = done
	onPartial := se.onPartial
	se.mu.Unlock()

	finish := func(result *SpeculativeResult) {
		se.mu.Lock()
		if se.state == SpecRunning {
			se.result = result
			if result != nil {
				se.state = SpecReady
			} else {
				se.state = SpecIdle
			}
		}
		se.cancel = nil
		se.mu.Unlock()
		close(done)
	}

	go func() {
		defer sCancel()
		defer func() {
			if r := recover(); r != nil {
				finish(nil)
			}
		}()

		sttResult, err := orch.TranscribeRaw(sCtx, audio, lang)
		if err != nil || sCtx.Err() != nil {
			finish(nil)
			return
		}

		partial := strings.TrimSpace(sttResult.Text)
		if partial == "" || len(partial) < 2 {
			finish(nil)
			return
		}

		if onPartial != nil {
			onPartial(partial)
		}

		if orch.llm == nil {
			// STT-only speculation (SpeculativeLLM disabled) — still useful
			// to callers that only want the early partial transcript.
			finish(&SpeculativeResult{PartialTranscript: partial})
			return
		}

		messages := append(append([]Message{}, historySnapshot...), Message{Role: "user", Content: partial})
		response, err := orch.llm.Complete(sCtx, messages, tools)
		if err != nil || sCtx.Err() != nil || strings.TrimSpace(response) == "" {
			// STT succeeded but generation didn't — still record the
			// transcript so Await's caller at least knows speculation ran,
			// even though there's no response to use.
			finish(&SpeculativeResult{PartialTranscript: partial})
			return
		}

		finish(&SpeculativeResult{PartialTranscript: partial, Response: response})
	}()
}

// Await waits (bounded by ctx) for the current or most recent speculative
// run to finish, then returns its response IF the run's own transcript
// matches finalTranscript (case/whitespace-insensitive) AND it actually
// got as far as generating a response. Returns ("", false) on any
// mismatch, failure, or timeout — the caller's only correct reaction to
// that is to run the normal, real pipeline exactly as if speculation had
// never been attempted; Await never blocks a caller with no speculation
// in flight (state==SpecIdle returns immediately).
func (se *SpeculativeExecutor) Await(ctx context.Context, finalTranscript string) (string, bool) {
	se.mu.Lock()
	state := se.state
	done := se.done
	se.mu.Unlock()

	if state == SpecIdle {
		return "", false
	}
	if state == SpecRunning {
		select {
		case <-done:
		case <-ctx.Done():
			return "", false
		}
	}

	se.mu.Lock()
	result := se.result
	se.mu.Unlock()

	if result == nil || result.Response == "" {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(result.PartialTranscript), strings.TrimSpace(finalTranscript)) {
		return "", false
	}
	return result.Response, true
}

// Cancel aborts any in-flight speculative run and clears its result — used
// when new audio arrives that invalidates whatever guess was in progress
// (the user kept talking past the pause that triggered it).
func (se *SpeculativeExecutor) Cancel() {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.cancel != nil {
		se.cancel()
		se.cancel = nil
	}
	se.state = SpecIdle
	se.result = nil
}
