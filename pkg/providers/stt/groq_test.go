package stt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestGroqSTT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := struct {
			Text string `json:"text"`
		}{
			Text: "groq transcription",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	s := &GroqSTT{
		apiKey:     "test-key",
		url:        server.URL,
		model:      "whisper-large-v3-turbo",
		sampleRate: 44100,
	}

	result, err := s.Transcribe(context.Background(), []byte{0}, orchestrator.LanguageEn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "groq transcription" {
		t.Errorf("expected 'groq transcription', got '%s'", result)
	}

	s.SetSampleRate(16000)
	if s.sampleRate != 16000 {
		t.Errorf("expected 16000, got %d", s.sampleRate)
	}

	if s.Name() != "groq-stt" {
		t.Errorf("expected groq-stt, got %s", s.Name())
	}
}
