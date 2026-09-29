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
