package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// RemoteLLM is a language model that lives in the client: the process that holds the voice session's socket
// answers each request itself, so its model and its tools can be anywhere (a laptop, a private network, a
// LangGraph in the same process) and nothing has to be reachable from outside. Lokutor keeps the speech side.
//
// It speaks the same OpenAI chat protocol as CustomLLM, over the session's own socket:
//
//	server -> client  {"type":"llm_request","request_id":"r1","messages":[...],"tools":[...]}
//	client -> server  {"type":"llm_chunk","request_id":"r1","data":"text so far..."}            (any number)
//	                  {"type":"llm_tool_call","request_id":"r1","id":"c1","name":"...","arguments":"{...}"}
//	                  {"type":"llm_done","request_id":"r1"}   or   {"type":"llm_error","request_id":"r1","data":"why"}
//	server -> client  {"type":"llm_cancel","request_id":"r1"}   when the caller spoke over the reply
//
// A tool call is executed by Lokutor when it names one of the tools sent in the request, and the next request
// carries the result as a tool message, exactly as with an HTTPS endpoint. A client that runs its own tools
// sends text only and asks for no tools.
type RemoteLLM struct {
	send    func(map[string]interface{}) error
	noTools bool

	mu      sync.Mutex
	pending map[string]chan remoteEvent
	seq     atomic.Int64

	// FirstEventTimeout is how long a request may wait for the client's first event; IdleTimeout, between events.
	FirstEventTimeout time.Duration
	IdleTimeout       time.Duration
}

type remoteEvent struct {
	kind string // chunk, tool_call, done, error
	text string
	call orchestrator.ToolCallEventData
}

// NewRemoteLLM returns a provider that sends requests with send (the session's socket writer). noTools never
// offers the agent's tools to the client.
func NewRemoteLLM(send func(map[string]interface{}) error, noTools bool) *RemoteLLM {
	return &RemoteLLM{send: send, noTools: noTools, pending: map[string]chan remoteEvent{},
		FirstEventTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second}
}

func (l *RemoteLLM) Name() string { return "remote-llm" }

// Deliver hands the provider a message the client sent (llm_chunk, llm_tool_call, llm_done, llm_error). It never
// blocks: the session's socket loop calls it between audio frames. A message for a request nobody waits for
// any more (cancelled, finished) is dropped.
func (l *RemoteLLM) Deliver(msg map[string]interface{}) {
	id, _ := msg["request_id"].(string)
	l.mu.Lock()
	ch := l.pending[id]
	l.mu.Unlock()
	if ch == nil {
		return
	}
	ev := remoteEvent{}
	switch msg["type"] {
	case "llm_chunk":
		ev.kind = "chunk"
		ev.text, _ = msg["data"].(string)
	case "llm_tool_call":
		ev.kind = "tool_call"
		ev.call.CallID, _ = msg["id"].(string)
		ev.call.Name, _ = msg["name"].(string)
		switch a := msg["arguments"].(type) {
		case string:
			ev.call.Arguments = a
		case nil:
			ev.call.Arguments = "{}"
		default:
			b, _ := json.Marshal(a)
			ev.call.Arguments = string(b)
		}
	case "llm_done":
		ev.kind = "done"
	case "llm_error":
		ev.kind = "error"
		ev.text, _ = msg["data"].(string)
	default:
		return
	}
	select {
	case ch <- ev:
	default:
		// A client that floods faster than the turn reads: end the request rather than stall the socket loop.
		select {
		case ch <- remoteEvent{kind: "error", text: "too many events buffered"}:
		default:
		}
	}
}

func (l *RemoteLLM) run(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool,
	onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, []orchestrator.ToolCallEventData, error) {
	id := fmt.Sprintf("r%d", l.seq.Add(1))
	ch := make(chan remoteEvent, 512)
	l.mu.Lock()
	l.pending[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()

	req := map[string]interface{}{"type": "llm_request", "request_id": id, "messages": messages}
	if len(tools) > 0 && !l.noTools {
		req["tools"] = tools
	}
	if err := l.send(req); err != nil {
		return "", nil, fmt.Errorf("remote llm: could not reach the client: %w", err)
	}

	var full strings.Builder
	var calls []orchestrator.ToolCallEventData
	wait := l.FirstEventTimeout
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = l.send(map[string]interface{}{"type": "llm_cancel", "request_id": id}) // the caller spoke over it: stop generating
			return full.String(), calls, ctx.Err()
		case <-timer.C:
			_ = l.send(map[string]interface{}{"type": "llm_cancel", "request_id": id})
			return full.String(), calls, fmt.Errorf("remote llm: the client did not answer within %s", wait)
		case ev := <-ch:
			timer.Stop()
			wait = l.IdleTimeout
			switch ev.kind {
			case "chunk":
				if ev.text != "" {
					full.WriteString(ev.text)
					if onChunk != nil {
						if err := onChunk(ev.text); err != nil {
							return "", nil, err
						}
					}
				}
			case "tool_call":
				if ev.call.Name != "" {
					calls = append(calls, ev.call)
				}
			case "error":
				return full.String(), calls, fmt.Errorf("remote llm: the client reported: %s", ev.text)
			case "done":
				return full.String(), calls, nil
			}
		}
	}
}

func (l *RemoteLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool,
	onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	text, calls, err := l.run(ctx, messages, tools, onChunk, onToolCall)
	if err != nil {
		return text, err
	}
	if onToolCall != nil {
		for _, c := range calls {
			if err := onToolCall(c); err != nil {
				return "", err
			}
		}
	}
	return text, nil
}

func (l *RemoteLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	text, calls, err := l.run(ctx, messages, tools, nil, nil)
	if err != nil {
		return "", err
	}
	oc := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		var t openAIToolCall
		t.ID, t.Function.Name = c.CallID, c.Name
		t.Function.Arguments = json.RawMessage(mustJSONString(c.Arguments))
		oc = append(oc, t)
	}
	captureToolCalls(ctx, oc)
	return text, nil
}

func mustJSONString(s string) []byte { b, _ := json.Marshal(s); return b }
