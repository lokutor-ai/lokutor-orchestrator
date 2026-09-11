package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestManagedStream_ToolLoopAbortRecoversState covers the tool-call
// infinite-loop guard's abort path in handleNonStreamingToolCalls
// (managed_stream_ext.go) — previously only the happy multi-round-chain path
// was tested (nonstreaming_tool_chain_test.go); the abort branch itself
// (ConversationSession.RecordToolCall's per-tool cap being hit mid-chain)
// had no coverage. This is exactly the kind of "should never happen but
// isn't verified" branch the task calls out: if a future change dropped the
// setIdle() call or the RecordToolCall check here, a runaway tool-call chain
// would leave the stream wedged in StateProcessing forever — the caller
// would just hear silence for the rest of the call, since
// monitorInactivity's stuck-state recovery net only covers StateInterrupted,
// not StateProcessing.
func TestManagedStream_ToolLoopAbortRecoversState(t *testing.T) {
	marker := func(id string) string {
		return fmt.Sprintf(`[TOOL_CALLS] [{"id":%q,"type":"function","function":{"name":"loop_tool","arguments":"{}"}}]`, id)
	}
	// RecordToolCall allows 3 calls per tool per session; the 4th round's
	// marker is what must trip the loop guard.
	llm := &MockNonStreamingLLM{responses: []string{marker("c0"), marker("c1"), marker("c2"), marker("c3")}}

	stt := &MockSTTProvider{transcribeResult: "loop please"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})

	var loopCalls atomic.Int32
	orch.RegisterTool("loop_tool", func(args string) (string, error) {
		loopCalls.Add(1)
		return `{"ok": true}`, nil
	})

	session := NewConversationSession("tool-loop-test")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "loop please")

	var errMsg string
	timeout := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev := <-ms.Events():
			if ev.Type == ErrorEvent {
				if s, ok := ev.Data.(string); ok {
					errMsg = s
				}
				break loop
			}
		case <-timeout:
			t.Fatal("timed out waiting for the tool-loop-detected ErrorEvent — the call would hang silently in production")
		}
	}

	if !strings.Contains(errMsg, "Tool loop detected") {
		t.Fatalf("expected a 'Tool loop detected' error, got: %q", errMsg)
	}
	if got := loopCalls.Load(); got != 3 {
		t.Fatalf("expected the tool handler to run exactly 3 times (the allowed budget) before the abort, got %d", got)
	}

	// The stream must recover to Idle, not stay wedged in Processing.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if ms.getState() == StateIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected state to recover to Idle after the tool-loop abort, got %v", ms.getState())
}

// TestManagedStream_IdleInterruptIsSafeAndRecoverable covers Interrupt()
// called with no pipeline or barge-in active at all (state genuinely Idle) —
// a plausible real sequencing case (e.g. a transport-layer "stop" signal
// arriving right as a turn finishes, or a duplicate interrupt control
// message) that none of the existing interrupt tests exercise; they all
// interrupt mid-Speaking/Processing. Confirms this doesn't panic, doesn't
// fabricate a bogus Interrupted event, and — because handleInterrupt
// unconditionally sets ms.state = StateInterrupted regardless of what was
// actually running — that the next real VAD-detected utterance (the actual
// production trigger for a new turn) still recovers the stream instead of
// leaving it wedged.
func TestManagedStream_IdleInterruptIsSafeAndRecoverable(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "hi there"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := New(stt, llm, tts, cfg)
	session := NewConversationSession("idle-interrupt-test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if got := stream.getState(); got != StateIdle {
		t.Fatalf("expected a freshly created stream to start Idle, got %v", got)
	}

	stream.Interrupt()

	select {
	case ev := <-stream.Events():
		if ev.Type == Interrupted {
			t.Fatal("Interrupt() on a truly idle stream should not emit an Interrupted event — nothing was interrupted")
		}
	case <-time.After(200 * time.Millisecond):
	}

	if got := stream.getState(); got != StateInterrupted {
		t.Fatalf("expected state to be StateInterrupted after Interrupt() (documented current behavior), got %v", got)
	}

	// The next real VAD start must still recover the stream.
	stream.onVADStart(stream.getState())
	if got := stream.getState(); got != StateListening {
		t.Fatalf("stream did not recover from an idle Interrupt(): expected StateListening after the next VAD start, got %v", got)
	}
}
