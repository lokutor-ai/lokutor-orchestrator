package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type AssemblyAISTT struct {
	apiKey string
}

func NewAssemblyAISTT(apiKey string) *AssemblyAISTT {
	return &AssemblyAISTT{
		apiKey: apiKey,
	}
}

func (s *AssemblyAISTT) Name() string {
	return "assemblyai-stt"
}

func (s *AssemblyAISTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	
	uploadURL, err := s.upload(ctx, audioPCM)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	
	transcriptID, err := s.submit(ctx, uploadURL, lang)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	
	for {
		select {
		case <-ctx.Done():
			return orchestrator.TranscriptionResult{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			res, status, err := s.getTranscript(ctx, transcriptID)
			if err != nil {
				return orchestrator.TranscriptionResult{}, err
			}
			if status == "completed" {
				return res, nil
			}
			if status == "error" {
				return orchestrator.TranscriptionResult{}, fmt.Errorf("assemblyai transcription failed")
			}
		}
	}
}

func (s *AssemblyAISTT) upload(ctx context.Context, audioPCM []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.assemblyai.com/v2/upload", bytes.NewReader(audioPCM))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		UploadURL string `json:"upload_url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.UploadURL, nil
}

func (s *AssemblyAISTT) submit(ctx context.Context, uploadURL string, lang orchestrator.Language) (string, error) {
	payload := map[string]interface{}{
		"audio_url": uploadURL,
	}
	if lang != "" {
		payload["language_code"] = string(lang)
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.assemblyai.com/v2/transcript", bytes.NewReader(body))
	req.Header.Set("Authorization", s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.ID, nil
}

func (s *AssemblyAISTT) getTranscript(ctx context.Context, id string) (orchestrator.TranscriptionResult, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.assemblyai.com/v2/transcript/"+id, nil)
	req.Header.Set("Authorization", s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return orchestrator.TranscriptionResult{}, "", err
	}
	defer resp.Body.Close()

	var result struct {
		Status     string  `json:"status"`
		Text       string  `json:"text"`
		Confidence float64 `json:"confidence"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return orchestrator.TranscriptionResult{
		Text:         result.Text,
		NoSpeechProb: 1.0 - result.Confidence,
	}, result.Status, nil
}
