package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type OpenAILLM struct {
	apiKey string
	url    string
	model  string
}

func NewOpenAILLM(apiKey string, model string) *OpenAILLM {
	if model == "" {
		model = "gpt-4o"
	}
	return &OpenAILLM{
		apiKey: apiKey,
		url:    "https://api.openai.com/v1/chat/completions",
		model:  model,
	}
}

func (l *OpenAILLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	payload := map[string]interface{}{
		"model":    l.model,
		"messages": messages,
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
	req.Header.Set("Authorization", "Bearer "+l.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("openai llm error (status %d): %v", resp.StatusCode, errResp)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices returned from openai")
	}

	return result.Choices[0].Message.Content, nil
}

func (l *OpenAILLM) Name() string {
	return "openai-llm"
}
