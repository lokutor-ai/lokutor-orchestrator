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

func TestGoogleLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "key=test-key") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}{
			Candidates: []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			}{
				{
					Content: struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					}{
						Parts: []struct {
							Text string `json:"text"`
						}{
							{Text: "hello from google"},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	l := &GoogleLLM{
		apiKey: "test-key",
		url:    server.URL,
		model:  "gemini",
	}

	messages := []orchestrator.Message{
		{Role: "user", Content: "hi"},
	}

	resp, err := l.Complete(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp != "hello from google" {
		t.Errorf("expected 'hello from google', got '%s'", resp)
	}
}
