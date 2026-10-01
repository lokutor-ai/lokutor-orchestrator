package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type MockStreamingLLM struct {
	responses []struct {
		content   string
		toolCalls []ToolCallEventData
	}
	callCount int
}

func (m *MockStreamingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	if m.callCount >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return resp.content, nil
}

func (m *MockStreamingLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	if m.callCount >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++

	if resp.content != "" {
		if onChunk != nil {
			onChunk(resp.content)
		}
	}

	for _, tc := range resp.toolCalls {
		if onToolCall != nil {
			onToolCall(tc)
		}
	}

	return resp.content, nil
}

func (m *MockStreamingLLM) Name() string { return "MockStreamingLLM" }

func TestManagedStream_ToolCalling(t *testing.T) {
	llm := &MockStreamingLLM{
		responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{
				content: "",
				toolCalls: []ToolCallEventData{
					{Name: "get_weather", Arguments: `{"location":"Madrid"}`, CallID: "c1"},
				},
			},
			{
				content:   "The weather in Madrid is sunny.",
				toolCalls: nil,
			},
		},
	}

	stt := &MockSTTProvider{transcribeResult: "whats the weather?"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}

	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})

	// Tool handlers run in their own goroutine (see dispatchToolCall) — a
	// plain bool written there and read from the test goroutine below raced
	// under -race even though BotResponse happens to be emitted afterward,
	// since a channel receive only establishes happens-before for the
	// sender's own goroutine, not for a separate handler goroutine joined via
	// a WaitGroup the test never observes. atomic.Bool makes the flag itself
	// safe to read from either goroutine regardless of ordering.
	var weatherCalled atomic.Bool
	orch.RegisterTool("get_weather", func(args string) (string, error) {
		weatherCalled.Store(true)
		var params struct{ Location string }
		json.Unmarshal([]byte(args), &params)
		return fmt.Sprintf("It is currently sunny in %s", params.Location), nil
	})

	session := NewConversationSession("test_user")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	// Trigger response
	go ms.runLLMAndTTS(context.Background(), "whats the weather?")

	// We wait for BotResponse which is always emitted after
	// session.AddMessage("assistant", response) completes.
	timeout := time.After(2 * time.Second)
	var events []EventType
loop:
	for {
		select {
		case ev := <-ms.Events():
			events = append(events, ev.Type)
			if ev.Type == BotResponse {
				break loop
			}
		case <-timeout:
			t.Fatalf("Timed out waiting for events. Got: %v", events)
		}
	}

	if !weatherCalled.Load() {
		t.Error("get_weather tool was never called")
	}

	// Verify conversation history has tool result
	ctx := session.GetContextCopy()
	hasToolMsg := false
	for _, m := range ctx {
		if m.Role == "tool" {
			hasToolMsg = true
			if !strings.Contains(m.Content, "sunny") {
				t.Errorf("Unexpected tool result: %s", m.Content)
			}
		}
	}
	if !hasToolMsg {
		t.Error("Tool result message not found in session context")
	}

	foundFinalResponse := false
	for _, m := range ctx {
		if m.Role == "assistant" && strings.Contains(m.Content, "weather in Madrid is sunny") {
			foundFinalResponse = true
		}
	}
	if !foundFinalResponse {
		t.Error("Final assistant response not found in session context")
	}
}

// A tool turn the caller has talked over must not speak its reply once the tool finishes: a newer
// turn has the floor and answers with the exchange in its context. On 2026-10-01 a booking for one
// turn finished while the next turn was answering, and both replies were spoken.
func TestReplyAfterToolsIsNotSpokenOnceANewerTurnHasTheFloor(t *testing.T) {
	llm := &MockStreamingLLM{responses: []struct {
		content   string
		toolCalls []ToolCallEventData
	}{
		{toolCalls: []ToolCallEventData{{Name: "book_appointment", Arguments: `{}`, CallID: "c1"}}},
		{content: "Listo, reservado."},
	}}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}, nil, DefaultConfig(), &NoOpLogger{})
	release := make(chan struct{})
	orch.RegisterTool("book_appointment", func(args string) (string, error) {
		<-release
		return `{"booked": true}`, nil
	})
	session := NewConversationSession("u")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "sí, resérvalo")
	timeout := time.After(2 * time.Second)
	for waiting := true; waiting; {
		select {
		case ev := <-ms.Events():
			waiting = ev.Type != ToolCall
		case <-timeout:
			t.Fatal("no tool call")
		}
	}
	// The caller speaks again and a newer turn takes the floor while the booking runs.
	ms.mu.Lock()
	ms.payloadGen++
	ms.mu.Unlock()
	close(release)

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-ms.Events():
			if ev.Type == BotResponse {
				t.Fatalf("the superseded turn spoke its reply: %v", ev.Data)
			}
		case <-deadline:
			if llm.callCount != 1 {
				t.Fatalf("the reply round should not run for a superseded turn, LLM called %d times", llm.callCount)
			}
			hasTool := false
			for _, m := range session.GetContextCopy() {
				hasTool = hasTool || m.Role == "tool"
			}
			if !hasTool {
				t.Fatal("the tool exchange must stay in the context for the newer turn")
			}
			return
		}
	}
}
