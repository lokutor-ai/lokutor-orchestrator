package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestAnthropicLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var req struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
			System   string              `json:"system,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if req.System != "system instructions" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}{
			Content: []struct {
				Text string `json:"text"`
			}{
				{Text: "hello from anthropic"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	l := &AnthropicLLM{
		apiKey: "test-key",
		url:    server.URL,
		model:  "claude-3",
	}

	messages := []orchestrator.Message{
		{Role: "system", Content: "system instructions"},
		{Role: "user", Content: "hi"},
	}

	resp, err := l.Complete(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp != "hello from anthropic" {
		t.Errorf("expected 'hello from anthropic', got '%s'", resp)
	}
}
