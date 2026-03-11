package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type AnthropicLLM struct {
	apiKey string
	url    string
	model  string
}

func NewAnthropicLLM(apiKey string, model string) *AnthropicLLM {
	if model == "" {
		model = "claude-3-5-sonnet-20240620"
	}
	return &AnthropicLLM{
		apiKey: apiKey,
		url:    "https://api.anthropic.com/v1/messages",
		model:  model,
	}
}

func (l *AnthropicLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	
	var system string
	var anthropicMessages []map[string]string

	for _, msg := range messages {
		if msg.Role == "system" {
			system = msg.Content
		} else {
			anthropicMessages = append(anthropicMessages, map[string]string{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	payload := map[string]interface{}{
		"model":      l.model,
		"messages":   anthropicMessages,
		"max_tokens": 1024,
	}
	if system != "" {
		payload["system"] = system
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", l.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", l.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("anthropic llm error (status %d): %v", resp.StatusCode, errResp)
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("no content returned from anthropic")
	}

	return result.Content[0].Text, nil
}

func (l *AnthropicLLM) Name() string {
	return "anthropic-llm"
}
