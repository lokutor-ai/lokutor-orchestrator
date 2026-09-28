package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func toolCallMsg(content string, ids ...string) Message {
	calls := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, map[string]interface{}{
			"id": id, "type": "function",
			"function": map[string]interface{}{"name": "f", "arguments": "{}"},
		})
	}
	return Message{Role: "assistant", Content: content, ToolCalls: calls}
}

func toolResultMsg(id string) Message {
	return Message{Role: "tool", Content: `{"ok":true}`, ToolCallID: id, Name: "f"}
}

func TestPairedToolMessages(t *testing.T) {
	user := Message{Role: "user", Content: "track my order"}
	reply := Message{Role: "assistant", Content: "It ships Tuesday."}

	valid := []Message{user, toolCallMsg("", "a", "b"), toolResultMsg("b"), toolResultMsg("a"), reply}
	out, dropped := pairedToolMessages(valid)
	assert.Equal(t, valid, out, "a call answered once per id, in any order, is kept whole")
	assert.Zero(t, dropped)

	// The 2026-09-29 shape: a chained round recorded its result without the call.
	out, dropped = pairedToolMessages([]Message{user, toolCallMsg("", "a"), toolResultMsg("a"), toolResultMsg("c2"), reply})
	assert.Equal(t, []Message{user, toolCallMsg("", "a"), toolResultMsg("a"), reply}, out)
	assert.Equal(t, 1, dropped)

	// A call left unanswered takes its partial results with it; its words stay.
	out, dropped = pairedToolMessages([]Message{user, toolCallMsg("Let me check.", "a", "b"), toolResultMsg("a"), reply})
	assert.Equal(t, []Message{user, {Role: "assistant", Content: "Let me check."}, reply}, out)
	assert.Equal(t, 1, dropped)

	// A result naming another call (dropped, and its call with it), and a call at the very end with
	// no result.
	out, dropped = pairedToolMessages([]Message{user, toolCallMsg("", "a"), toolResultMsg("z"), reply, toolCallMsg("", "b")})
	assert.Equal(t, []Message{user, reply}, out)
	assert.Equal(t, 3, dropped)
}

// recordingStreamingLLM answers from a script and keeps every request it was sent.
type recordingStreamingLLM struct {
	mu     sync.Mutex
	script []struct {
		text  string
		calls []ToolCallEventData
	}
	requests [][]Message
}

func (m *recordingStreamingLLM) next(messages []Message) (string, []ToolCallEventData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, messages)
	if len(m.requests) > len(m.script) {
		return "", nil
	}
	r := m.script[len(m.requests)-1]
	return r.text, r.calls
}

func (m *recordingStreamingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	text, _ := m.next(messages)
	return text, nil
}

func (m *recordingStreamingLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	text, calls := m.next(messages)
	if text != "" {
		if err := onChunk(text); err != nil {
			return "", err
		}
	}
	for _, tc := range calls {
		if err := onToolCall(tc); err != nil {
			return "", err
		}
	}
	return text, nil
}

func (m *recordingStreamingLLM) Name() string { return "recordingStreamingLLM" }

// A tool call made after the first round's results must be recorded with its call, and the model
// asked again for the answer. Round two used to append a bare tool message (Cerebras then rejected
// every later request of the call) and speak "Got it." without asking.
func TestManagedStream_ChainedToolCallsStayPairedAndAreAnswered(t *testing.T) {
	llm := &recordingStreamingLLM{script: []struct {
		text  string
		calls []ToolCallEventData
	}{
		{calls: []ToolCallEventData{{Name: "find_order", Arguments: `{"name":"Dana"}`, CallID: "c1"}}},
		{calls: []ToolCallEventData{{Name: "track_order", Arguments: `{"order_id":"PO999"}`, CallID: "c2"}}},
		{text: "Your order PO999 ships Tuesday."},
	}}
	stt := &MockSTTProvider{transcribeResult: "where is my order"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	orch.RegisterTool("find_order", func(string) (string, error) { return `{"order_id":"PO999"}`, nil })
	orch.RegisterTool("track_order", func(string) (string, error) { return `{"eta":"Tuesday"}`, nil })

	session := NewConversationSession("chained-tools")
	session.SetTools([]Tool{
		{Type: "function", Function: map[string]interface{}{"name": "find_order"}},
		{Type: "function", Function: map[string]interface{}{"name": "track_order"}},
	})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "where is my order")

	var spoken string
	timeout := time.After(3 * time.Second)
	for spoken == "" {
		select {
		case ev := <-ms.Events():
			switch ev.Type {
			case BotResponse:
				spoken, _ = ev.Data.(string)
			case ErrorEvent:
				t.Fatalf("turn failed: %v", ev.Data)
			}
		case <-timeout:
			t.Fatal("timed out waiting for the answer after the chained tool call")
		}
	}
	assert.Equal(t, "Your order PO999 ships Tuesday.", spoken)

	session.mu.RLock()
	raw := append([]Message(nil), session.Context...)
	session.mu.RUnlock()
	_, dropped := pairedToolMessages(raw)
	assert.Zero(t, dropped, "every tool result in the history follows the call that asked for it")

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if assert.Len(t, llm.requests, 3) {
		last := llm.requests[2]
		var ids []string
		for _, m := range last {
			if m.Role == "tool" {
				ids = append(ids, m.ToolCallID)
			}
		}
		assert.Equal(t, []string{"c1", "c2"}, ids, "the final request carries both results")
	}
}

// Past the per-tool cap the model is told so and still answers; the turn used to fail outright.
func TestManagedStream_ChainedToolCapAnswersInsteadOfFailing(t *testing.T) {
	call := func(id string) []ToolCallEventData {
		return []ToolCallEventData{{Name: "search", Arguments: `{}`, CallID: id}}
	}
	llm := &recordingStreamingLLM{script: []struct {
		text  string
		calls []ToolCallEventData
	}{
		{calls: call("c1")}, {calls: call("c2")}, {calls: call("c3")}, {calls: call("c4")},
		{text: "I couldn't find it, sorry."},
	}}
	stt := &MockSTTProvider{transcribeResult: "find it"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	var mu sync.Mutex
	ran := 0
	orch.RegisterTool("search", func(string) (string, error) {
		mu.Lock()
		ran++
		mu.Unlock()
		return `{"results":[]}`, nil
	})

	session := NewConversationSession("chained-cap")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "search"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "find it")

	var spoken string
	timeout := time.After(3 * time.Second)
	for spoken == "" {
		select {
		case ev := <-ms.Events():
			switch ev.Type {
			case BotResponse:
				spoken, _ = ev.Data.(string)
			case ErrorEvent:
				t.Fatalf("turn failed: %v", ev.Data)
			}
		case <-timeout:
			t.Fatal("timed out waiting for the answer after the tool cap")
		}
	}
	assert.Equal(t, "I couldn't find it, sorry.", spoken)
	mu.Lock()
	assert.Equal(t, 3, ran, "the fourth call is refused, not run")
	mu.Unlock()
}

func TestToolCallInSpeech(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: map[string]interface{}{"name": "search_apartments"}},
		{Type: "function", Function: map[string]interface{}{"name": "search"}},
	}
	for _, s := range []string{
		`tool: search_apartments arguments: bedrooms 3`,
		`{"city": "Atlanta", "bedrooms": 3}`,
		`Search_Apartments with city Atlanta.`,
	} {
		assert.True(t, toolCallInSpeech(s, tools), "%q", s)
	}
	for _, s := range []string{
		"Sure thing, looking for three-bedroom places in Atlanta now.",
		"Let me search for that.", // a tool name without an underscore is also a word
		"Your order ships Tuesday.",
	} {
		assert.False(t, toolCallInSpeech(s, tools), "%q", s)
	}
}

func TestUnspeakableReply(t *testing.T) {
	tools := []Tool{{Type: "function", Function: map[string]interface{}{"name": "add_to_cart"}}}
	// Logged on 2026-09-29.
	for _, s := range []string{
		"Sure, here's a desk around three hundred dollars.We have product PROD1 price ninety-nine point nine nine, which is under three hundred.",
		"Add it.We should call add_to_cart.We need to add product PROD1.",
		`{"product_id":"PROD1","quantity":1}Done, one hiking boots Premium is now in your cart.`,
	} {
		assert.NotEmpty(t, unspeakableReply(s, tools), "%q", s)
	}
	for _, s := range []string{
		"It's ninety-nine ninety-nine. Want me to add it?",
		"Your flight leaves at 4 p.m. on Friday.",
		"That's 3.14 percent, about U.S. average.",
		"¿Te lo añado al carrito? Vale.",
	} {
		assert.Empty(t, unspeakableReply(s, tools), "%q", s)
	}
}

// A reply that turns out to be a written-out tool call is stopped before it is spoken, and the model
// is asked again; the second answer, a real tool call, is made.
func TestManagedStream_WrittenOutToolCallIsRetried(t *testing.T) {
	llm := &recordingStreamingLLM{script: []struct {
		text  string
		calls []ToolCallEventData
	}{
		{text: "Sure thing.We should call add_to_cart.We need to add it."},
		{calls: []ToolCallEventData{{Name: "add_to_cart", Arguments: `{"product_id":"PROD1"}`, CallID: "c1"}}},
		{text: "Done, it's in your cart."},
	}}
	stt := &MockSTTProvider{transcribeResult: "add it"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	orch := NewWithAllLayers(stt, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	var added sync.WaitGroup
	added.Add(1)
	orch.RegisterTool("add_to_cart", func(string) (string, error) { added.Done(); return `{"status":"success"}`, nil })

	session := NewConversationSession("written-out")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "add_to_cart"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	go ms.runLLMAndTTS(context.Background(), "add it")

	var spoken string
	timeout := time.After(3 * time.Second)
	for spoken == "" {
		select {
		case ev := <-ms.Events():
			switch ev.Type {
			case BotResponse:
				spoken, _ = ev.Data.(string)
			case ErrorEvent:
				t.Fatalf("turn failed: %v", ev.Data)
			}
		case <-timeout:
			t.Fatal("timed out waiting for the answer after the retried tool call")
		}
	}
	added.Wait()
	assert.Equal(t, "Done, it's in your cart.", spoken)
	session.mu.RLock()
	defer session.mu.RUnlock()
	for _, m := range session.Context {
		assert.NotContains(t, m.Content, "We should call", "the unspoken reply is not recorded")
	}
}
