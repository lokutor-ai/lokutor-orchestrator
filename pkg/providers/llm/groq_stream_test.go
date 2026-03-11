package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestGroqLLM_StreamComplete_ToolCalls(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Simulate Groq SSE for tool calls - using RAW bytes to avoid any escaping issues in Go strings
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loca"}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"tion\":\"Madrid\"}"}}]}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()

	l := NewGroqLLM("fake_key", "meta-llama/llama-4-scout-17b-16e-instruct")
	l.url = ts.URL

	var toolCalls []orchestrator.ToolCallEventData
	content, err := l.StreamComplete(context.Background(), nil, nil, nil, func(tc orchestrator.ToolCallEventData) error {
		toolCalls = append(toolCalls, tc)
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if content != "" {
		t.Errorf("Expected empty content, got %q", content)
	}

	if len(toolCalls) != 1 {
		t.Fatalf("Expected 1 tool call, got %d. Tool calls: %+v", len(toolCalls), toolCalls)
	}

	tc := toolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("Expected name get_weather, got %s", tc.Name)
	}
	if tc.Arguments != `{"location":"Madrid"}` {
		t.Errorf("Expected arguments {\"location\":\"Madrid\"}, got %q", tc.Arguments)
	}
	if tc.CallID != "call_1" {
		t.Errorf("Expected ID call_1, got %s", tc.CallID)
	}
}

func TestGroqLLM_StreamComplete_MultipleToolCalls(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f1","arguments":""}},{"index":1,"id":"c2","function":{"name":"f2","arguments":""}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}},{"index":1,"function":{"arguments":"{\"b\":2}"}}]}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()

	l := NewGroqLLM("fake_key", "model")
	l.url = ts.URL

	var toolCalls []orchestrator.ToolCallEventData
	_, err := l.StreamComplete(context.Background(), nil, nil, nil, func(tc orchestrator.ToolCallEventData) error {
		toolCalls = append(toolCalls, tc)
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(toolCalls) != 2 {
		t.Fatalf("Expected 2 tool calls, got %d", len(toolCalls))
	}

	if toolCalls[0].Name != "f1" || toolCalls[1].Name != "f2" {
		t.Errorf("Unexpected tool names: %s, %s", toolCalls[0].Name, toolCalls[1].Name)
	}
}

func TestGroqLLM_StreamComplete_Text(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"Hello"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":" world"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer ts.Close()

	l := NewGroqLLM("fake_key", "model")
	l.url = ts.URL

	var receivedChunks []string
	content, err := l.StreamComplete(context.Background(), nil, nil, func(chunk string) error {
		receivedChunks = append(receivedChunks, chunk)
		return nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if content != "Hello world" {
		t.Errorf("Expected Hello world, got %q", content)
	}

	if len(receivedChunks) != 2 {
		t.Errorf("Expected 2 chunks, got %d", len(receivedChunks))
	}
}
