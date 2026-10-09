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

// When the host ends the call (the account can no longer pay for it), the model is told why, is offered
// NO tools (on 2026-10-09 it was offered end_call on a session without one, wrote the call out as text, and
// said nothing), and the request is logged as a farewell; the channel closes once the goodbye is done.
func TestFarewellTellsTheModelWhyAndOffersNoTools(t *testing.T) {
	llm := &toolRecordingLLM{got: make(chan struct{}, 1)}
	orch := NewWithAllLayers(&MockSTTProvider{}, llm, &MockTTSProvider{synthesizeResult: []byte("audio")}, nil, DefaultConfig(), &NoOpLogger{})
	session := NewConversationSession("farewell")
	session.SetTools([]Tool{
		{Type: "function", Function: map[string]interface{}{"name": "book_appointment"}},
		{Type: "function", Function: map[string]interface{}{"name": "end_call"}},
	})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	note := "This call has to end now. Say a short goodbye."
	done := ms.Farewell(note, 2*time.Second)
	if done == nil {
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
	if last.Role != "user" || !strings.Contains(last.Content, "has to end now") {
		t.Fatalf("the model was not told why: last message %+v", last)
	}
	if len(llm.tools[0]) != 0 {
		t.Fatalf("tools offered for the goodbye: %+v, want none", llm.tools[0])
	}
	llm.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the farewell's channel never closed")
	}
	llm.mu.Lock()
	if responseTriggerFor(farewellTrigger) != "farewell" {
		t.Fatal("the farewell is not logged as one")
	}
}

func TestFarewellOnAClosedStreamIsRefused(t *testing.T) {
	orch := NewWithAllLayers(&MockSTTProvider{}, &toolRecordingLLM{got: make(chan struct{}, 1)}, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("closed"))
	ms.Close()
	if ms.Farewell("x", time.Second) != nil {
		t.Fatal("Farewell accepted on a closed stream")
	}
}
