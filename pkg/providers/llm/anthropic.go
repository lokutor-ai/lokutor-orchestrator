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
	if len(tools) > 0 {
		// Anthropic expects tools in a specific format: {"name", "description", "input_schema"}
		var anthropicTools []map[string]interface{}
		for _, t := range tools {
			if fn, ok := t.Function.(map[string]interface{}); ok {
				// The function map uses OpenAI-style {name, description, parameters}.
				// Map to Anthropic's {name, description, input_schema} format.
				inputSchema, _ := fn["parameters"].(map[string]interface{})
				if inputSchema == nil {
					inputSchema = map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					}
				}
				anthropicTools = append(anthropicTools, map[string]interface{}{
					"name":         fn["name"],
					"description":  fn["description"],
					"input_schema": inputSchema,
				})
			}
		}
		payload["tools"] = anthropicTools
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
			Text    string `json:"text"`
			Type    string `json:"type"`
			ID      string `json:"id"`
			Name    string `json:"name"`
			Input   interface{} `json:"input"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("no content returned from anthropic")
	}

	// If the model requested tool use, serialize it as JSON so the caller can dispatch.
	var toolUses []map[string]interface{}
	for _, block := range result.Content {
		if block.Type == "tool_use" {
			toolUses = append(toolUses, map[string]interface{}{
				"id":    block.ID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      block.Name,
					"arguments": block.Input,
				},
			})
		}
	}
	if len(toolUses) > 0 {
		b, err := json.Marshal(toolUses)
		if err == nil {
			return fmt.Sprintf(`[TOOL_CALLS] %s`, string(b)), nil
		}
	}

	return result.Content[0].Text, nil
}

func (l *AnthropicLLM) Name() string {
	return "anthropic-llm"
}
