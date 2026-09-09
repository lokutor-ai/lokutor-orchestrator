package orchestrator

import (
	"context"
	"testing"
	"time"
)

func newTestOrchestrator(sttText, llmText string) *Orchestrator {
	stt := &MockSTTProvider{transcribeResult: sttText}
	llm := &MockLLMProvider{completeResult: llmText}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)
	return NewWithVAD(stt, llm, tts, vad, DefaultConfig())
}

// TestSpeculator_MatchingTranscriptReturnsResponse is the core promise of
// speculative execution: if the eventually-confirmed transcript matches
// what was speculated on, Await hands back the already-generated response
// instead of making the caller wait for a fresh LLM call.
func TestSpeculator_MatchingTranscriptReturnsResponse(t *testing.T) {
	orch := newTestOrchestrator("hello there", "General Kenobi")
	se := NewSpeculativeExecutor(50)

	se.Start(context.Background(), orch, []byte{1, 2, 3, 4}, LanguageEn, nil, nil)

	response, ok := se.Await(context.Background(), "hello there")
	if !ok {
		t.Fatal("expected Await to return the speculative response on a matching transcript")
	}
	if response != "General Kenobi" {
		t.Fatalf("got response %q, want %q", response, "General Kenobi")
	}
}

// TestSpeculator_MismatchedTranscriptMisses covers the safety property this
// whole feature depends on: a wrong guess must never be usable — the
// caller falls through to the normal, real pipeline instead.
func TestSpeculator_MismatchedTranscriptMisses(t *testing.T) {
	orch := newTestOrchestrator("hello there", "General Kenobi")
	se := NewSpeculativeExecutor(50)

	se.Start(context.Background(), orch, []byte{1, 2, 3, 4}, LanguageEn, nil, nil)

	_, ok := se.Await(context.Background(), "goodbye now")
	if ok {
		t.Fatal("expected Await to miss when the confirmed transcript differs from what was speculated on")
	}
}

// TestSpeculator_IdleReturnsImmediately ensures a caller with no
// speculation in flight never blocks waiting for one — this is what makes
// trySpeculativeResponse safe to call unconditionally before every
// runLLMAndTTS.
func TestSpeculator_IdleReturnsImmediately(t *testing.T) {
	se := NewSpeculativeExecutor(50)
	start := time.Now()
	_, ok := se.Await(context.Background(), "anything")
	if ok {
		t.Fatal("expected Await to miss when no speculation was ever started")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Await on an idle executor took %v, expected near-instant", elapsed)
	}
}

// TestSpeculator_CancelResetsForNextRun mirrors trySpeculativeResponse's
// cleanup: after consuming (or missing) a result, the executor must go
// back to Idle so the next utterance's trigger isn't permanently blocked.
func TestSpeculator_CancelResetsForNextRun(t *testing.T) {
	orch := newTestOrchestrator("hello there", "General Kenobi")
	se := NewSpeculativeExecutor(50)

	se.Start(context.Background(), orch, []byte{1, 2, 3, 4}, LanguageEn, nil, nil)
	se.Await(context.Background(), "hello there")
	se.Cancel()

	if !se.ShouldSpeculateOnPause(time.Time{}) {
		t.Fatal("expected the executor to be speculate-again-ready after Cancel")
	}
}

// TestSpeculator_STTOnlyWhenLLMDisabled covers running with a nil LLM
// provider on the orchestrator (SpeculativeLLM effectively off at the
// provider level) — should still surface the partial transcript via
// onPartial but never claim a usable response.
func TestSpeculator_STTOnlyWhenLLMDisabled(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)
	orch := NewWithVAD(stt, nil, tts, vad, DefaultConfig())

	se := NewSpeculativeExecutor(50)
	var gotPartial string
	se.SetOnPartial(func(t string) { gotPartial = t })

	se.Start(context.Background(), orch, []byte{1, 2}, LanguageEn, nil, nil)
	_, ok := se.Await(context.Background(), "hi")
	if ok {
		t.Fatal("expected no usable response when the orchestrator has no LLM provider")
	}
	if gotPartial != "hi" {
		t.Fatalf("expected onPartial to fire with the STT result, got %q", gotPartial)
	}
}
