package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type DeepgramSTT struct {
	apiKey string
	url    string
}

func NewDeepgramSTT(apiKey string) *DeepgramSTT {
	return &DeepgramSTT{
		apiKey: apiKey,
		url:    "https://api.deepgram.com/v1/listen",
	}
}

func (s *DeepgramSTT) Name() string {
	return "deepgram-stt"
}

func (s *DeepgramSTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (orchestrator.TranscriptionResult, error) {
	u, err := url.Parse(s.url)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	params := u.Query()
	params.Set("model", "nova-2")
	params.Set("smart_format", "true")
	if lang != "" {
		params.Set("language", string(lang))
	}
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(audioPCM))
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	req.Header.Set("Authorization", "Token "+s.apiKey)
	req.Header.Set("Content-Type", "audio/l16; rate=44100; channels=1") 

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return orchestrator.TranscriptionResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return orchestrator.TranscriptionResult{}, fmt.Errorf("deepgram error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
					Confidence float64 `json:"confidence"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return orchestrator.TranscriptionResult{}, err
	}

	if len(result.Results.Channels) == 0 || len(result.Results.Channels[0].Alternatives) == 0 {
		return orchestrator.TranscriptionResult{}, nil
	}

	alt := result.Results.Channels[0].Alternatives[0]
	return orchestrator.TranscriptionResult{
		Text:         alt.Transcript,
		NoSpeechProb: 1.0 - alt.Confidence,
	}, nil
}
