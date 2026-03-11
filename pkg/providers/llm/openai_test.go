package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestOpenAILLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var req struct {
			Model    string                 `json:"model"`
			Messages []orchestrator.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{
					Message: struct {
						Content string `json:"content"`
					}{Content: "hello from openai"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	l := &OpenAILLM{
		apiKey: "test-key",
		url:    server.URL,
		model:  "gpt-4o",
	}

	messages := []orchestrator.Message{
		{Role: "user", Content: "hi"},
	}

	resp, err := l.Complete(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp != "hello from openai" {
		t.Errorf("expected 'hello from openai', got '%s'", resp)
	}

	if l.Name() != "openai-llm" {
		t.Errorf("expected openai-llm, got %s", l.Name())
	}
}
