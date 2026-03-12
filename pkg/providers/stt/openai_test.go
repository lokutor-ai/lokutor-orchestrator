package stt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestOpenAISTT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := struct {
			Text string `json:"text"`
		}{
			Text: "transcribed text",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	s := &OpenAISTT{
		apiKey:     "test-key",
		url:        server.URL,
		model:      "whisper-1",
		sampleRate: 44100,
	}

	result, err := s.Transcribe(context.Background(), []byte{0, 0, 0, 0}, orchestrator.LanguageEn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Text != "transcribed text" {
		t.Errorf("expected 'transcribed text', got '%s'", result.Text)
	}

	if s.Name() != "openai_stt" {
		t.Errorf("expected openai_stt, got %s", s.Name())
	}
}
