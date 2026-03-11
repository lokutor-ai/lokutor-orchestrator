package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestGroqLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
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
					}{Content: "hello from groq"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	l := &GroqLLM{
		apiKey: "test-key",
		url:    server.URL,
		model:  "meta-llama/llama-4-scout-17b-16e-instruct",
	}

	messages := []orchestrator.Message{
		{Role: "user", Content: "hi"},
	}

	resp, err := l.Complete(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp != "hello from groq" {
		t.Errorf("expected 'hello from groq', got '%s'", resp)
	}

	if l.Name() != "groq-llm" {
		t.Errorf("expected groq-llm, got %s", l.Name())
	}
}
