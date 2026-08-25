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
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
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
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices returned from openai")
	}

	// If the model requested tool calls, serialize them as JSON so the caller
	// can dispatch them (the orchestrator's non-streaming path handles these).
	if len(result.Choices[0].Message.ToolCalls) > 0 {
		tcList := make([]map[string]interface{}, 0, len(result.Choices[0].Message.ToolCalls))
		for _, tc := range result.Choices[0].Message.ToolCalls {
			tcList = append(tcList, map[string]interface{}{
				"id": tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		b, err := json.Marshal(tcList)
		if err == nil {
			return fmt.Sprintf(`[TOOL_CALLS] %s`, string(b)), nil
		}
	}

	return result.Choices[0].Message.Content, nil
}

func (l *OpenAILLM) Name() string {
	return "openai-llm"
}
