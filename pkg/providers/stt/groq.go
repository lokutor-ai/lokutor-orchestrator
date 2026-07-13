package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

type GroqSTT struct {
	apiKey     string
	url        string
	model      string
	sampleRate int

	maxRetries int
	baseWait   time.Duration
}

func NewGroqSTT(apiKey string, model string) *GroqSTT {
	if model == "" {
		model = "whisper-large-v3-turbo"
	}
	s := &GroqSTT{
		apiKey:     apiKey,
		url:        "https://api.groq.com/openai/v1/audio/transcriptions",
		model:      model,
		sampleRate: 16000,
		maxRetries: 3,
		baseWait:   1 * time.Second,
	}
	if v := os.Getenv("GROQ_STT_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.maxRetries = n
		}
	}
	if v := os.Getenv("GROQ_STT_RETRY_BASE_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			s.baseWait = d
		}
	}
	return s
}

func (s *GroqSTT) SetSampleRate(rate int) {
	s.sampleRate = rate
}

func (s *GroqSTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	wavData := audio.NewWavBuffer(audioPCM, s.sampleRate)

	bodyBuf := &bytes.Buffer{}
	writer := multipart.NewWriter(bodyBuf)

	if err := writer.WriteField("model", s.model); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if lang != "" && lang != "na" {
		if err := writer.WriteField("language", string(lang)); err != nil {
			return orchestrator.TranscriptionResult{}, err
		}
	}

	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}
	if _, err := io.Copy(part, bytes.NewReader(wavData)); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if err := writer.Close(); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	bodyBytes := bodyBuf.Bytes()
	contentType := writer.FormDataContentType()

	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			wait := s.baseWait*time.Duration(1<<uint(attempt-1)) + time.Duration(rand.Intn(500))*time.Millisecond
			select {
			case <-ctx.Done():
				return orchestrator.TranscriptionResult{}, ctx.Err()
			case <-time.After(wait):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(bodyBytes))
		if err != nil {
			return orchestrator.TranscriptionResult{}, err
		}

		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+s.apiKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var result struct {
				Text     string `json:"text"`
				Segments []struct {
					NoSpeechProb float64 `json:"no_speech_prob"`
				} `json:"segments"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
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

		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("groq stt rate limited (status 429)")
			continue
		}
		if resp.StatusCode == 503 {
			lastErr = fmt.Errorf("groq stt service unavailable (status 503)")
			continue
		}

		return orchestrator.TranscriptionResult{}, fmt.Errorf("groq stt error (status %d): %s", resp.StatusCode, string(body))
	}

	return orchestrator.TranscriptionResult{}, fmt.Errorf("groq stt error after %d retries: %v", s.maxRetries, lastErr)
}

func (s *GroqSTT) Name() string {
	return "groq-stt"
}
