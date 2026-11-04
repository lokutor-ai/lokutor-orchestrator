package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, l := range lines {
		w.Write([]byte("data: " + l + "\n\n"))
	}
	w.Write([]byte("data: [DONE]\n\n"))
}

func TestCustomLLMStreamsTextAndToolCalls(t *testing.T) {
	var gotAuth, gotTenant, gotPath string
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTenant, gotPath = r.Header.Get("Authorization"), r.Header.Get("X-Tenant"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w,
			`{"choices":[{"delta":{"content":"Hola, "}}]}`,
			`{"choices":[{"delta":{"content":"Ana."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"book","arguments":"{\"na"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"me\":\"Ana\"}"}}]}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer srv.Close()

	l := NewCustomLLM(srv.URL+"/v1", "sekret", "my-agent", map[string]string{"X-Tenant": "acme"}, srv.Client(), false)
	var text strings.Builder
	var calls []orchestrator.ToolCallEventData
	tools := []orchestrator.Tool{{Type: "function", Function: map[string]interface{}{"name": "book"}}}
	out, err := l.StreamComplete(context.Background(), []orchestrator.Message{{Role: "user", Content: "hi"}}, tools,
		func(s string) error { text.WriteString(s); return nil },
		func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hola, Ana." || text.String() != "Hola, Ana." {
		t.Fatalf("text %q / %q", out, text.String())
	}
	if len(calls) != 1 || calls[0].Name != "book" || calls[0].Arguments != `{"name":"Ana"}` || calls[0].CallID != "c1" {
		t.Fatalf("tool calls %+v", calls)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sekret" || gotTenant != "acme" {
		t.Fatalf("request: %s %q %q", gotPath, gotAuth, gotTenant)
	}
	if body["model"] != "my-agent" || body["stream"] != true || body["tools"] == nil {
		t.Fatalf("payload %v", body)
	}
}

func TestCustomLLMNoToolsAndOwnAuthHeader(t *testing.T) {
	var body map[string]interface{}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	l := NewCustomLLM(srv.URL, "ignored", "", map[string]string{"Authorization": "Token abc"}, srv.Client(), true)
	tools := []orchestrator.Tool{{Type: "function", Function: map[string]interface{}{"name": "x"}}}
	if _, err := l.StreamComplete(context.Background(), nil, tools, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := body["tools"]; has {
		t.Fatal("tools must not be sent when switched off")
	}
	if _, has := body["model"]; has {
		t.Fatal("no model, none sent")
	}
	if auth != "Token abc" {
		t.Fatalf("a configured Authorization header wins over the key: %q", auth)
	}
}

func TestCustomLLMAnswersInOneJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"Todo listo.","tool_calls":[{"id":"t","type":"function","function":{"name":"end_call","arguments":{}}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	l := NewCustomLLM(srv.URL, "", "m", nil, srv.Client(), false)
	var calls []orchestrator.ToolCallEventData
	out, err := l.StreamComplete(context.Background(), nil, nil, nil, func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	if err != nil || out != "Todo listo." {
		t.Fatalf("%q %v", out, err)
	}
	if len(calls) != 1 || calls[0].Name != "end_call" || calls[0].Arguments != "{}" {
		t.Fatalf("an object-shaped arguments value must be accepted: %+v", calls)
	}
}

func TestCustomLLMRetriesWithoutStreamOptions(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		n++
		if _, has := b["stream_options"]; has {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"detail":"extra field stream_options not permitted"}`))
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"vale"}}]}`)
	}))
	defer srv.Close()
	l := NewCustomLLM(srv.URL, "", "", nil, srv.Client(), false)
	out, err := l.StreamComplete(context.Background(), nil, nil, nil, nil)
	if err != nil || out != "vale" || n != 2 {
		t.Fatalf("%q %v after %d requests", out, err, n)
	}
}

func TestCustomLLMReportsTheEndpointsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	l := NewCustomLLM(srv.URL, "", "", nil, srv.Client(), false)
	_, err := l.StreamComplete(context.Background(), nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("the status and body must reach the log: %v", err)
	}
}
