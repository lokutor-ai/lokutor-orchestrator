package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type OpenAISTT struct {
	apiKey     string
	url        string
	model      string
	sampleRate int
}

func NewOpenAISTT(apiKey string, model string) *OpenAISTT {
	if model == "" {
		model = "whisper-1"
	}
	return &OpenAISTT{
		apiKey:     apiKey,
		url:        "https://api.openai.com/v1/audio/transcriptions",
		model:      model,
		sampleRate: 44100,
	}
}

func (s *OpenAISTT) SetSampleRate(rate int) {
	s.sampleRate = rate
}

func (s *OpenAISTT) Name() string {
	return "openai_stt"
}

func (s *OpenAISTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	wavData := audio.NewWavBuffer(audioPCM, s.sampleRate)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("model", s.model); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if lang != "" {
		if err := writer.WriteField("language", string(lang)); err != nil {
			return orchestrator.TranscriptionResult{}, err
		}
	}

	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}
	if _, err := part.Write(wavData); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", s.url, body)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return orchestrator.TranscriptionResult{}, fmt.Errorf("openai error: %s (status %d)", string(respBody), resp.StatusCode)
	}

	var result struct {
		Text     string `json:"text"`
		Segments []struct {
			NoSpeechProb float64 `json:"no_speech_prob"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	maxNoSpeech := 0.0
	if len(result.Segments) > 0 {
		for _, seg := range result.Segments {
			if seg.NoSpeechProb > maxNoSpeech {
				maxNoSpeech = seg.NoSpeechProb
			}
		}
	}

	return orchestrator.TranscriptionResult{
		Text:         result.Text,
		NoSpeechProb: maxNoSpeech,
	}, nil
}
