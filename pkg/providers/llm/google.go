package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type GoogleLLM struct {
	apiKey string
	url    string
	model  string
}

func NewGoogleLLM(apiKey string, model string) *GoogleLLM {
	if model == "" {
		model = "gemini-1.5-flash"
	}
	return &GoogleLLM{
		apiKey: apiKey,
		url:    "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent",
		model:  model,
	}
}

func (l *GoogleLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	type GoogleMessage struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}

	var googleMessages []GoogleMessage
	for _, m := range messages {
		role := m.Role
		if role == "system" {
			role = "user" 
		}
		if role == "assistant" {
			role = "model"
		}
		msg := GoogleMessage{Role: role}
		msg.Parts = append(msg.Parts, struct {
			Text string `json:"text"`
		}{Text: m.Content})
		googleMessages = append(googleMessages, msg)
	}

	payload := map[string]interface{}{
		"contents": googleMessages,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", l.url+"?key="+l.apiKey, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("google llm error (status %d): %v", resp.StatusCode, errResp)
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no response from google llm")
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}

func (l *GoogleLLM) Name() string {
	return "google-llm"
}
