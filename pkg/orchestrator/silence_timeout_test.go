package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingLLM wraps MockLLMProvider to count Complete() calls, so the test
// can distinguish "fired once" from "looped forever" without depending on
// TTS/audio side effects.
type countingLLM struct {
	result string
	calls  atomic.Int32
}

func (c *countingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	c.calls.Add(1)
	return c.result, nil
}
func (c *countingLLM) Name() string { return "CountingLLM" }

// TestMonitorInactivity_FiresOnceNotForever reproduces the production bug
// where the silence-timeout nudge (ms.runLLMAndTTS("[USER_SILENCE_TIMEOUT]"))
// re-fired every ~2s tick indefinitely while the user stayed silent, each
// time asking the LLM for a fresh paraphrase of "are you there" — observed
// live as the bot repeating slightly-reworded variants of the same message
// every ~10-12s with no user speech in between. It must fire at most once
// per idle stretch, then go quiet until real speech resets the gate.
func TestMonitorInactivity_FiresOnceNotForever(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &countingLLM{result: "still there?"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 50 * time.Millisecond // well under the 2s ticker interval
	// DefaultConfig's FirstSpeaker is Bot, which schedules its own
	// runLLMAndTTS call for the greeting (~600ms in) — that's a real,
	// separate code path from the silence-timeout nudge under test here,
	// and would otherwise get double-counted as if it were the nudge.
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Idle, no user speech: the first ticker pass (up to 2s) should fire
	// exactly one nudge, then stay quiet no matter how many more ticks pass.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && llm.calls.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := llm.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 LLM call after the first silence timeout, got %d", got)
	}

	// Wait through several more 2s ticker intervals — this is exactly the
	// window the old code would have refired in every cycle.
	time.Sleep(4500 * time.Millisecond)
	if got := llm.calls.Load(); got != 1 {
		t.Fatalf("silence nudge fired again without new user speech (got %d calls) — the one-shot gate regressed", got)
	}

	// Simulate the user actually speaking again: this must re-arm the gate
	// so a later, genuine silence still gets its own nudge.
	stream.mu.Lock()
	stream.silenceNudgeSent = false
	stream.state = StateIdle
	stream.lastActivityAt = time.Now().Add(-time.Second) // already stale
	stream.mu.Unlock()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && llm.calls.Load() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := llm.calls.Load(); got != 2 {
		t.Fatalf("expected a second nudge after the gate was re-armed by new activity, got %d calls", got)
	}
}
