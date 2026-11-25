package llm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// fakeClient plays the SDK: it reads the provider's outgoing messages and answers through Deliver.
type fakeClient struct {
	l    *RemoteLLM
	mu   sync.Mutex
	sent []map[string]interface{}
	on   func(c *fakeClient, req map[string]interface{})
}

func newFake(noTools bool, on func(c *fakeClient, req map[string]interface{})) (*RemoteLLM, *fakeClient) {
	c := &fakeClient{on: on}
	c.l = NewRemoteLLM(func(m map[string]interface{}) error {
		c.mu.Lock()
		c.sent = append(c.sent, m)
		c.mu.Unlock()
		if m["type"] == "llm_request" {
			go c.on(c, m)
		}
		return nil
	}, noTools)
	return c.l, c
}

func (c *fakeClient) reply(req map[string]interface{}, kind string, extra map[string]interface{}) {
	m := map[string]interface{}{"type": kind, "request_id": req["request_id"]}
	for k, v := range extra {
		m[k] = v
	}
	c.l.Deliver(m)
}

func (c *fakeClient) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.sent {
		out = append(out, m["type"].(string))
	}
	return out
}

func TestRemoteLLMStreamsTextAndToolCalls(t *testing.T) {
	l, c := newFake(false, func(c *fakeClient, req map[string]interface{}) {
		c.reply(req, "llm_chunk", map[string]interface{}{"data": "Hola, "})
		c.reply(req, "llm_chunk", map[string]interface{}{"data": "Ana."})
		c.reply(req, "llm_tool_call", map[string]interface{}{"id": "c1", "name": "book", "arguments": `{"name":"Ana"}`})
		c.reply(req, "llm_done", nil)
	})
	var chunks []string
	var calls []orchestrator.ToolCallEventData
	tools := []orchestrator.Tool{{Type: "function", Function: map[string]interface{}{"name": "book"}}}
	out, err := l.StreamComplete(context.Background(), []orchestrator.Message{{Role: "user", Content: "hi"}}, tools,
		func(s string) error { chunks = append(chunks, s); return nil },
		func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	if err != nil || out != "Hola, Ana." || len(chunks) != 2 {
		t.Fatalf("%q %v %v", out, err, chunks)
	}
	if len(calls) != 1 || calls[0].Name != "book" || calls[0].Arguments != `{"name":"Ana"}` || calls[0].CallID != "c1" {
		t.Fatalf("calls %+v", calls)
	}
	req := c.sent[0]
	if req["type"] != "llm_request" || req["tools"] == nil || req["messages"] == nil {
		t.Fatalf("request %v", req)
	}
}

func TestRemoteLLMOffersNoToolsWhenSwitchedOff(t *testing.T) {
	l, c := newFake(true, func(c *fakeClient, req map[string]interface{}) {
		c.reply(req, "llm_chunk", map[string]interface{}{"data": "ok"})
		c.reply(req, "llm_done", nil)
	})
	tools := []orchestrator.Tool{{Type: "function", Function: map[string]interface{}{"name": "x"}}}
	if _, err := l.StreamComplete(context.Background(), nil, tools, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := c.sent[0]["tools"]; has {
		t.Fatal("tools must not be sent")
	}
}

func TestRemoteLLMCancelsWhenTheCallerSpeaksOver(t *testing.T) {
	l, c := newFake(false, func(c *fakeClient, req map[string]interface{}) {
		c.reply(req, "llm_chunk", map[string]interface{}{"data": "Un momento"})
		// then never finishes
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := l.StreamComplete(ctx, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("a cancelled request returns the context's error")
	}
	time.Sleep(20 * time.Millisecond)
	if ty := c.types(); ty[len(ty)-1] != "llm_cancel" {
		t.Fatalf("the client must be told to stop: %v", ty)
	}
}

func TestRemoteLLMClientErrorAndTimeout(t *testing.T) {
	l, _ := newFake(false, func(c *fakeClient, req map[string]interface{}) {
		c.reply(req, "llm_error", map[string]interface{}{"data": "graph exploded"})
	})
	if _, err := l.StreamComplete(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "graph exploded") {
		t.Fatalf("the client's reason must reach the log: %v", err)
	}
	l2, c2 := newFake(false, func(c *fakeClient, req map[string]interface{}) {}) // never answers
	l2.FirstEventTimeout = 80 * time.Millisecond
	start := time.Now()
	if _, err := l2.StreamComplete(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("a silent client must time out: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("took too long")
	}
	if ty := c2.types(); ty[len(ty)-1] != "llm_cancel" {
		t.Fatalf("%v", ty)
	}
}

func TestRemoteLLMIgnoresLateAndUnknownMessages(t *testing.T) {
	l, _ := newFake(false, func(c *fakeClient, req map[string]interface{}) {
		c.reply(req, "llm_chunk", map[string]interface{}{"data": "a"})
		c.reply(req, "llm_done", nil)
	})
	if out, err := l.StreamComplete(context.Background(), nil, nil, nil, nil); err != nil || out != "a" {
		t.Fatalf("%q %v", out, err)
	}
	l.Deliver(map[string]interface{}{"type": "llm_chunk", "request_id": "r1", "data": "late"}) // no waiter: dropped, no panic
	l.Deliver(map[string]interface{}{"type": "nonsense", "request_id": "r9"})
}

// A client answers for as long as it likes too, and may send faster than the turn reads.
func TestRemoteLLMEndsAnAnswerWithoutEnd(t *testing.T) {
	chunk := strings.Repeat("a", 1<<20)
	for name, kind := range map[string]string{"text": "llm_chunk", "arguments": "llm_tool_call"} {
		t.Run(name, func(t *testing.T) {
			l, _ := newFake(false, func(c *fakeClient, req map[string]interface{}) {
				for i := 0; i < 64; i++ {
					c.reply(req, kind, map[string]interface{}{"data": chunk, "name": "x", "arguments": chunk})
					time.Sleep(time.Millisecond)
				}
				c.reply(req, "llm_done", nil)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := l.StreamComplete(ctx, nil, nil, nil, func(orchestrator.ToolCallEventData) error { return nil })
			if err == nil || ctx.Err() != nil || len(out) > maxCustomAnswer {
				t.Fatalf("took it whole: %d bytes, %v", len(out), err)
			}
		})
	}
	// Nobody reading: what waits is bounded too.
	release := make(chan struct{})
	l, c := newFake(false, func(c *fakeClient, req map[string]interface{}) {})
	go func() {
		_, _ = l.StreamComplete(context.Background(), nil, nil, func(string) error { <-release; return nil }, nil)
	}()
	for len(c.types()) == 0 {
		time.Sleep(time.Millisecond)
	}
	l.mu.Lock()
	ch := l.pending["r1"].ch
	l.mu.Unlock()
	req := map[string]interface{}{"request_id": "r1"}
	for i := 0; i < 64; i++ {
		c.reply(req, "llm_chunk", map[string]interface{}{"data": chunk})
	}
	close(release)
	if waiting := len(ch); waiting > maxCustomAnswer>>20+1 {
		t.Fatalf("%d MB waiting for a turn that does not read", waiting)
	}
}
