package orchestrator

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// MockNonStreamingLLM implements only Complete (no StreamComplete), matching
// the real Anthropic/OpenAI providers — it forces ManagedStream down the
// [TOOL_CALLS]-marker / handleNonStreamingToolCalls path instead of
// runStreamingLLM's onToolCall callback path.
type MockNonStreamingLLM struct {
	responses []string
	callCount int
}

func (m *MockNonStreamingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	if m.callCount >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return resp, nil
}

func (m *MockNonStreamingLLM) Name() string { return "MockNonStreamingLLM" }

// TestHandleNonStreamingToolCalls_ChainsMultipleRounds proves a second round
// of tool calls from a non-streaming provider (Anthropic/OpenAI's
// "[TOOL_CALLS] ..." marker convention) is actually executed instead of
// silently dropped. Before the fix, handleNonStreamingToolCalls made exactly
// one follow-up Complete() call after round one's tools ran, and if THAT
// call's response was itself another marker, the function just returned —
// no error, no speech, nothing: the caller heard silence for that turn.
func TestHandleNonStreamingToolCalls_ChainsMultipleRounds(t *testing.T) {
	llm := &MockNonStreamingLLM{
		responses: []string{
			// Round 1: model wants to check availability.
			`[TOOL_CALLS] [{"id":"c1","type":"function","function":{"name":"check_availability","arguments":"{\"date\":\"2026-09-15\"}"}}]`,
			// Round 2 (the follow-up after round 1's tool result): model now
			// wants to actually book it. This is the marker that used to be
			// silently discarded.
			`[TOOL_CALLS] [{"id":"c2","type":"function","function":{"name":"book_slot","arguments":"{\"date\":\"2026-09-15\"}"}}]`,
			// Round 3 (the follow-up after round 2's tool result): plain text,
			// no more tool calls — the actual spoken answer.
			"You're booked for September 15th.",
		},
	}

	stt := &MockSTTProvider{transcribeResult: "book me for the 15th"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}

	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})

	var availabilityCalled, bookCalled atomic.Bool
	orch.RegisterTool("check_availability", func(args string) (string, error) {
		availabilityCalled.Store(true)
		return `{"available": true}`, nil
	})
	orch.RegisterTool("book_slot", func(args string) (string, error) {
		bookCalled.Store(true)
		return `{"status": "booked"}`, nil
	})

	session := NewConversationSession("test_user")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "book me for the 15th")

	timeout := time.After(2 * time.Second)
	var events []EventType
	var gotFinalResponse bool
loop:
	for {
		select {
		case ev := <-ms.Events():
			events = append(events, ev.Type)
			if ev.Type == BotResponse {
				if text, ok := ev.Data.(string); ok && strings.Contains(text, "booked for September 15th") {
					gotFinalResponse = true
				}
				break loop
			}
			if ev.Type == ErrorEvent {
				t.Fatalf("Got ErrorEvent instead of a final response: %v", ev.Data)
			}
		case <-timeout:
			t.Fatalf("Timed out waiting for the round-2 tool chain to resolve. Got events: %v", events)
		}
	}

	if !availabilityCalled.Load() {
		t.Error("round 1 tool (check_availability) was never called")
	}
	if !bookCalled.Load() {
		t.Error("round 2 tool (book_slot) was never called — this is the bug: round-2+ tool calls were silently dropped")
	}
	if !gotFinalResponse {
		t.Error("did not receive the expected final spoken response after the tool chain")
	}
}
