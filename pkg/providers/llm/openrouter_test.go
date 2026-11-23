package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// The request is pinned to the configured hosts with no fallback, restricted to hosts that retain
// nothing, and turns the model's reasoning off -- and never carries gpt-oss's reasoning_effort.
func TestOpenRouterRequestShape(t *testing.T) {
	var got map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"Hola."}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()

	l := NewOpenRouterLLM("k", "deepseek/deepseek-v4.1-flash", []string{"wafer"}, "none")
	l.url = ts.URL
	tools := []orchestrator.Tool{{Type: "function"}}
	if _, err := l.StreamComplete(context.Background(), []orchestrator.Message{{Role: "user", Content: "hola"}}, tools, nil, nil); err != nil {
		t.Fatal(err)
	}
	prov, _ := got["provider"].(map[string]interface{})
	if prov == nil || prov["zdr"] != true || prov["data_collection"] != "deny" || prov["allow_fallbacks"] != false {
		t.Fatalf("provider routing = %v", got["provider"])
	}
	if only, _ := prov["only"].([]interface{}); len(only) != 1 || only[0] != "wafer" {
		t.Fatalf("provider.only = %v", prov["only"])
	}
	if r, _ := got["reasoning"].(map[string]interface{}); r == nil || r["enabled"] != false {
		t.Fatalf("reasoning = %v", got["reasoning"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort is gpt-oss's parameter and must not be sent")
	}
	if got["stream"] != true || got["tool_choice"] != "auto" {
		t.Fatalf("stream/tool_choice = %v / %v", got["stream"], got["tool_choice"])
	}
}

func TestOpenRouterStreamsContentAndToolCalls(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `: OPENROUTER PROCESSING`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"Un momento, "}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"lo reservo."}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"book","arguments":"{\"a\":"}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()

	l := NewOpenRouterLLM("k", "", []string{"wafer"}, "none")
	l.url = ts.URL
	var chunks []string
	var calls []orchestrator.ToolCallEventData
	content, err := l.StreamComplete(context.Background(), nil, nil,
		func(s string) error { chunks = append(chunks, s); return nil },
		func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if content != "Un momento, lo reservo." || len(chunks) != 2 {
		t.Fatalf("content %q chunks %v", content, chunks)
	}
	if len(calls) != 1 || calls[0].Name != "book" || calls[0].Arguments != `{"a":1}` || calls[0].CallID != "c1" {
		t.Fatalf("tool calls %+v", calls)
	}
}

// A provider failing after OpenRouter answered 200 arrives as an error chunk. It must be an error, so
// that a chain moves on to its next provider instead of treating an empty reply as the answer.
func TestOpenRouterErrorChunkIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"error":{"code":502,"message":"Provider returned error"}}`)
	}))
	defer ts.Close()
	l := NewOpenRouterLLM("k", "", nil, "")
	l.url = ts.URL
	if _, err := l.StreamComplete(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "Provider returned error") {
		t.Fatalf("err = %v, want the provider's error", err)
	}
}

func TestOpenRouterHTTPErrorIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"code":402,"message":"Insufficient credits"}}`)
	}))
	defer ts.Close()
	l := NewOpenRouterLLM("k", "", nil, "")
	l.url = ts.URL
	if _, err := l.StreamComplete(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "402") {
		t.Fatalf("err = %v, want status 402", err)
	}
}

// A max-tokens cap on the context reaches the request only when the model answers without reasoning first: a
// reasoning model would spend it thinking and return nothing.
func TestOpenRouterMaxTokensOnlyWithoutReasoning(t *testing.T) {
	ctx := orchestrator.WithMaxTokens(context.Background(), 16)
	off := NewOpenRouterLLM("k", "anthropic/claude-haiku-5.5", nil, "none")
	if got := off.withMaxTokens(ctx, off.payload(nil, nil, false))["max_tokens"]; got != 16 {
		t.Fatalf("reasoning off: max_tokens %v, want 16", got)
	}
	if _, ok := off.withMaxTokens(context.Background(), off.payload(nil, nil, false))["max_tokens"]; ok {
		t.Fatal("max_tokens sent without a cap on the context")
	}
	on := NewOpenRouterLLM("k", "deepseek/deepseek-v4.1-flash", nil, "")
	if _, ok := on.withMaxTokens(ctx, on.payload(nil, nil, false))["max_tokens"]; ok {
		t.Fatal("max_tokens sent to a model that may reason")
	}
}

// The hosts are an ordered list (order), fenced by only: only alone is an allowlist OpenRouter
// load-balances across, so "Europe, then the US" sent requests to the US with Europe up.
func TestOpenRouterHostsAreOrdered(t *testing.T) {
	l := NewOpenRouterLLM("k", "anthropic/claude-haiku-5.5", []string{"google-vertex/europe", "amazon-bedrock/eu-west-1"}, "none")
	b, _ := json.Marshal(l.payload(nil, nil, true))
	var got map[string]interface{}
	json.Unmarshal(b, &got)
	prov := got["provider"].(map[string]interface{})
	order, _ := prov["order"].([]interface{})
	only, _ := prov["only"].([]interface{})
	if len(order) != 2 || order[0] != "google-vertex/europe" || order[1] != "amazon-bedrock/eu-west-1" {
		t.Fatalf("provider.order = %v", prov["order"])
	}
	if len(only) != 2 || prov["allow_fallbacks"] != false || prov["zdr"] != true {
		t.Fatalf("provider = %v", prov)
	}
}

// An Anthropic model's request marks the system prompt, the history before the caller's latest turn
// and (top level) the whole request for the prompt cache; other models' requests carry no marks.
func TestOpenRouterPromptCacheMarks(t *testing.T) {
	msgs := []orchestrator.Message{
		{Role: "system", Content: "You are Lucía."},
		{Role: "assistant", Content: "Hola, ¿en qué puedo ayudarle?"},
		{Role: "user", Content: "Quiero reservar."},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{map[string]interface{}{"id": "c1"}}},
		{Role: "tool", Content: "{}", ToolCallID: "c1"},
		{Role: "assistant", Content: "¿Para qué día?"},
		{Role: "user", Content: "El martes."},
		{Role: "system", Content: "[Knowledge] martes abierto"},
	}
	l := NewOpenRouterLLM("k", "anthropic/claude-haiku-5.5", []string{"google-vertex/europe"}, "none")
	b, _ := json.Marshal(l.payload(msgs, nil, true))
	var got struct {
		CacheControl map[string]interface{}   `json:"cache_control"`
		Messages     []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.CacheControl["type"] != "ephemeral" {
		t.Fatalf("top-level cache_control = %v", got.CacheControl)
	}
	if len(got.Messages) != len(msgs) {
		t.Fatalf("%d messages, want %d", len(got.Messages), len(msgs))
	}
	marked := map[int]string{}
	for i, m := range got.Messages {
		if parts, ok := m["content"].([]interface{}); ok {
			p := parts[0].(map[string]interface{})
			if cc, _ := p["cache_control"].(map[string]interface{}); cc["type"] == "ephemeral" {
				marked[i] = p["text"].(string)
			}
		}
	}
	if len(marked) != 2 || marked[0] != "You are Lucía." || marked[5] != "¿Para qué día?" {
		t.Fatalf("marked = %v", marked)
	}
	if got.Messages[3]["tool_calls"] == nil || got.Messages[4]["tool_call_id"] != "c1" {
		t.Fatalf("unmarked messages changed: %v %v", got.Messages[3], got.Messages[4])
	}

	other := NewOpenRouterLLM("k", "deepseek/deepseek-v4.1-flash", []string{"wafer"}, "none")
	b, _ = json.Marshal(other.payload(msgs, nil, true))
	if strings.Contains(string(b), "cache_control") {
		t.Fatal("a non-Anthropic model got cache marks")
	}
}

// The default cap applies when the caller set none, and the caller's own cap wins.
func TestOpenRouterDefaultMaxTokens(t *testing.T) {
	l := NewOpenRouterLLM("k", "anthropic/claude-haiku-5.5", nil, "none")
	if p := l.withMaxTokens(context.Background(), map[string]interface{}{}); p["max_tokens"] != nil {
		t.Fatalf("no default set, max_tokens = %v", p["max_tokens"])
	}
	l.SetDefaultMaxTokens(2048)
	if p := l.withMaxTokens(context.Background(), map[string]interface{}{}); p["max_tokens"] != 2048 {
		t.Fatalf("default: max_tokens = %v", p["max_tokens"])
	}
	if p := l.withMaxTokens(orchestrator.WithMaxTokens(context.Background(), 40), map[string]interface{}{}); p["max_tokens"] != 40 {
		t.Fatalf("caller's cap: max_tokens = %v", p["max_tokens"])
	}
}

// The usage chunk's cache counts and cost reach the turn's sink and the session's meter, the prompt
// count stays whole (the customer's bill), and the serving host is recorded.
func TestOpenRouterRecordsCacheAndUpstream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"provider":"Amazon Bedrock","choices":[{"delta":{"content":"Vale."}}]}`)
		fmt.Fprintln(w, `data: {"provider":"Amazon Bedrock","choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":7,"total_tokens":5007,"cost":0.0001,"prompt_tokens_details":{"cached_tokens":4800,"cache_write_tokens":150}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()
	l := NewOpenRouterLLM("k", "anthropic/claude-haiku-5.5", []string{"amazon-bedrock/eu-west-1"}, "none")
	l.url = ts.URL
	u := &orchestrator.TokenUsage{}
	m := orchestrator.NewSessionMeter()
	ctx := orchestrator.WithSessionMeter(orchestrator.WithTokenUsage(context.Background(), u), m)
	if _, err := l.StreamComplete(ctx, []orchestrator.Message{{Role: "user", Content: "hola"}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p, _, _, _ := u.Snapshot(); p != 5000 {
		t.Fatalf("prompt tokens = %d, want the whole prompt", p)
	}
	if c := u.Cache(); c.CachedTokens != 4800 || c.CacheWriteTokens != 150 || c.CostUSD != 0.0001 {
		t.Fatalf("cache = %+v", c)
	}
	if u.Upstream() != "Amazon Bedrock" {
		t.Fatalf("upstream = %q", u.Upstream())
	}
	got := m.Snapshot()[orchestrator.TokensReply]
	if got.Prompt != 5000 || got.CachedPrompt != 4800 || got.CacheWrite != 150 || got.Requests != 1 {
		t.Fatalf("meter = %+v", got)
	}
}
