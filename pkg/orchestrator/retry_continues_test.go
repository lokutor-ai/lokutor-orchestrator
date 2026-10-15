package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// chunkedLLM streams each scripted reply in the pieces given, as a provider does, and keeps every request.
type chunkedLLM struct {
	mu       sync.Mutex
	script   [][]string
	requests [][]Message
	efforts  []string
}

func (m *chunkedLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "", nil
}

func (m *chunkedLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	m.mu.Lock()
	m.requests = append(m.requests, append([]Message(nil), messages...))
	m.efforts = append(m.efforts, ReasoningEffortFrom(ctx))
	n := len(m.requests)
	m.mu.Unlock()
	if n > len(m.script) {
		return "", nil
	}
	var all strings.Builder
	for _, c := range m.script[n-1] {
		all.WriteString(c)
		if err := onChunk(c); err != nil {
			return "", err
		}
	}
	return all.String(), nil
}

func (m *chunkedLLM) Name() string { return "chunkedLLM" }

// countingTTS records every piece of text it is asked to say.
type countingTTS struct {
	mu   sync.Mutex
	said []string
}

func (c *countingTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	c.mu.Lock()
	c.said = append(c.said, text)
	c.mu.Unlock()
	return []byte{1, 2, 3}, nil
}

func (c *countingTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	c.mu.Lock()
	c.said = append(c.said, text)
	c.mu.Unlock()
	return onChunk([]byte{1, 2, 3})
}

func (c *countingTTS) Abort() error { return nil }

func (c *countingTTS) Name() string { return "countingTTS" }

// A reply stopped at a written-out tool call after it had begun to be spoken: the second try is told what the caller
// already heard, asked at "medium", and the caller hears the goodbye once. On 2026-10-15 the second try started over
// and the goodbye was spoken twice.
func TestRetryAfterWrittenOutCallContinuesFromWhatWasSaid(t *testing.T) {
	llm := &chunkedLLM{script: [][]string{
		{"Lo siento, Manolo. ", "Que tengas un buen día. ", `{"name": "end_call", "arguments": {}}`},
		{""},
	}}
	tts := &countingTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "déjame en paz"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	session := NewConversationSession("retry-continues")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "end_call"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(context.Background(), "no quiero ninguna, déjame en paz"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish")
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.requests) != 2 {
		t.Fatalf("the model was asked %d times, want 2", len(llm.requests))
	}
	second := llm.requests[1]
	last := second[len(second)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "Que tengas un buen día.") || !strings.Contains(last.Content, "Lo siento, Manolo.") {
		t.Fatalf("the second try was not told what the caller heard; its request ends with %s: %q", last.Role, last.Content)
	}
	if llm.efforts[1] != "medium" {
		t.Errorf("second try asked at %q, want medium", llm.efforts[1])
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	joined := strings.Join(tts.said, " | ")
	if n := strings.Count(joined, "buen día"); n != 1 {
		t.Errorf("the goodbye was spoken %d times: %s", n, joined)
	}
}

// Words before a tool call are the acknowledgment and are said once, whether their sentence was already flushed to TTS
// when the call arrived or is still an unfinished tail. On 2026-10-16 both were said twice: the whole reply was spoken
// again as the filler, and the tail once more at the end of the turn.
func TestPreToolWordsSpokenOnce(t *testing.T) {
	for _, pre := range []string{"Un momento, lo miro.", "Un momento, lo miro"} {
		llm := &MockStreamingLLM{responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{content: pre, toolCalls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}}},
			{content: "Ya está.", toolCalls: nil},
		}}
		tts := &countingTTS{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
		orch.RegisterTool("lookup", func(args string) (string, error) { return `{"ok":true}`, nil })
		session := NewConversationSession("pre-tool-once")
		session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
		ms := orch.NewManagedStream(context.Background(), session)
		ms.runLLMAndTTS(context.Background(), "mira mi cita")
		deadline := time.Now().Add(3 * time.Second)
		for {
			tts.mu.Lock()
			joined := strings.Join(tts.said, " | ")
			tts.mu.Unlock()
			if strings.Contains(joined, "Ya está") || time.Now().After(deadline) {
				if n := strings.Count(joined, "lo miro"); n != 1 {
					t.Errorf("%q: the words before the call were said %d times: %s", pre, n, joined)
				}
				if !strings.Contains(joined, "Ya está") {
					t.Errorf("%q: the reply after the call was not said: %s", pre, joined)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		ms.Close()
	}
}
