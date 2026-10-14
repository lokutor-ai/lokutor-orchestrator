package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingLLM answers like MockNonStreamingLLM and keeps the messages of every call.
type recordingLLM struct {
	mu        sync.Mutex
	responses []string
	seen      [][]Message
}

func (m *recordingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, append([]Message(nil), messages...))
	if len(m.seen) > len(m.responses) {
		return "", nil
	}
	return m.responses[len(m.seen)-1], nil
}

func (m *recordingLLM) Name() string { return "recordingLLM" }

// What the agent says while its tools run is in the context the model answers from, with the calls. It used to be
// spoken and dropped, so the model was told the agent had called its tools in silence, and the reply after them opened
// with the same line again ("Un momento, lo miro." three times in one turn, 2026-10-15).
func TestWordsSpokenBeforeToolsAreInTheContext(t *testing.T) {
	llm := &recordingLLM{responses: []string{
		`[TOOL_CALLS] [{"id":"c1","type":"function","function":{"name":"check_order","arguments":"{\"id\":\"4512\"}"}}]`,
		"It shipped yesterday.",
	}}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "where is my order"}, llm,
		&MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}, nil, DefaultConfig(), &NoOpLogger{})
	orch.RegisterTool("check_order", func(args string) (string, error) { return `{"status": "shipped"}`, nil })
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("test_user"))
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "where is my order 4512")
	deadline := time.After(2 * time.Second)
wait:
	for {
		select {
		case ev := <-ms.Events():
			if ev.Type == BotResponse {
				break wait
			}
		case <-deadline:
			t.Fatal("no reply after the tool call")
		}
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.seen) < 2 {
		t.Fatalf("the model was asked %d times, want a second time after the tool", len(llm.seen))
	}
	var said string
	found := false
	for _, m := range llm.seen[1] {
		if m.Role == "assistant" && m.ToolCalls != nil {
			said, found = m.Content, true
		}
	}
	if !found {
		t.Fatal("no assistant message carrying the tool call in the follow-up context")
	}
	pool := toolFillerPool(ms.session.GetCurrentLanguage())
	inPool := false
	for _, p := range pool {
		if p == said {
			inPool = true
		}
	}
	if !inPool {
		t.Fatalf("the tool call's message says %q, want the filler the caller heard", said)
	}
}
