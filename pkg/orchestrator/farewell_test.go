package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// toolRecordingLLM records the messages and the tools of every streaming request.
type toolRecordingLLM struct {
	mu    sync.Mutex
	msgs  [][]Message
	tools [][]Tool
	got   chan struct{}
}

func (m *toolRecordingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "", nil
}

func (m *toolRecordingLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	m.mu.Lock()
	m.msgs = append(m.msgs, messages)
	m.tools = append(m.tools, tools)
	m.mu.Unlock()
	select {
	case m.got <- struct{}{}:
	default:
	}
	return "Lo siento, tengo que colgar. Adiós.", nil
}

func (m *toolRecordingLLM) Name() string { return "toolRecordingLLM" }

// When the host ends the call (the account can no longer pay for it), the model is told why, may only
// hang up, and the request is billed and logged as a farewell rather than as a caller's turn.
func TestFarewellTellsTheModelWhyAndOffersOnlyEndCall(t *testing.T) {
	llm := &toolRecordingLLM{got: make(chan struct{}, 1)}
	orch := NewWithAllLayers(&MockSTTProvider{}, llm, &MockTTSProvider{synthesizeResult: []byte("audio")}, nil, DefaultConfig(), &NoOpLogger{})
	session := NewConversationSession("farewell")
	session.SetTools([]Tool{
		{Type: "function", Function: map[string]interface{}{"name": "book_appointment"}},
		{Type: "function", Function: map[string]interface{}{"name": "end_call"}},
	})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	note := "The account behind this call has run out of credit. Say a short goodbye and call end_call."
	if !ms.Farewell(note, 2*time.Second) {
		t.Fatal("Farewell refused on an open stream")
	}
	select {
	case <-llm.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for the goodbye")
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	last := llm.msgs[0][len(llm.msgs[0])-1]
	if last.Role != "user" || !strings.Contains(last.Content, "run out of credit") {
		t.Fatalf("the model was not told why: last message %+v", last)
	}
	if len(llm.tools[0]) != 1 || toolFunctionName(llm.tools[0][0]) != "end_call" {
		t.Fatalf("tools offered for the goodbye: %+v, want only end_call", llm.tools[0])
	}
	if responseTriggerFor(farewellTrigger) != "farewell" {
		t.Fatal("the farewell is not logged as one")
	}
}

func TestFarewellOnAClosedStreamIsRefused(t *testing.T) {
	orch := NewWithAllLayers(&MockSTTProvider{}, &toolRecordingLLM{got: make(chan struct{}, 1)}, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("closed"))
	ms.Close()
	if ms.Farewell("x", time.Second) {
		t.Fatal("Farewell accepted on a closed stream")
	}
}

func TestToolFunctionName(t *testing.T) {
	type fn struct {
		Name string `json:"name"`
	}
	if toolFunctionName(Tool{Function: map[string]interface{}{"name": "a"}}) != "a" ||
		toolFunctionName(Tool{Function: fn{Name: "b"}}) != "b" ||
		toolFunctionName(Tool{}) != "" {
		t.Fatal("tool names not read")
	}
}
