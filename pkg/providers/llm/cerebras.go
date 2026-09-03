package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// CerebrasLLM talks to Cerebras Inference's OpenAI-compatible chat completions
// API (https://inference-docs.cerebras.ai). Cerebras runs on wafer-scale
// hardware specifically built for fast token generation — usually the
// fastest of the "fast inference" providers (Groq/Cerebras/SambaNova) — and
// its paid tier is plain usage-based billing with no manual tier-upgrade
// approval step, unlike Groq's Developer tier which gates on demand.
type CerebrasLLM struct {
	apiKey string
	url    string
	model  string
}

func NewCerebrasLLM(apiKey string, model string) *CerebrasLLM {
	if model == "" {
		model = "gpt-oss-120b"
	}
	return &CerebrasLLM{
		apiKey: apiKey,
		url:    "https://api.cerebras.ai/v1/chat/completions",
		model:  model,
	}
}

func (l *CerebrasLLM) Name() string { return "cerebras-llm" }

func (l *CerebrasLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
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
		return "", fmt.Errorf("cerebras api error (status %d): %v", resp.StatusCode, errResp)
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
		return "", fmt.Errorf("no response from cerebras")
	}
	return result.Choices[0].Message.Content, nil
}

func (l *CerebrasLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	payload := map[string]interface{}{
		"model":    l.model,
		"messages": messages,
		"stream":   true,
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
		return "", fmt.Errorf("cerebras api error (status %d): %v", resp.StatusCode, errResp)
	}

	reader := bufio.NewReader(resp.Body)
	var fullContent strings.Builder

	type toolCallState struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := make(map[int]*toolCallState)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			fullContent.WriteString(delta.Content)
			if onChunk != nil {
				if err := onChunk(delta.Content); err != nil {
					return "", err
				}
			}
		}

		for _, tc := range delta.ToolCalls {
			state, ok := toolCalls[tc.Index]
			if !ok {
				state = &toolCallState{}
				toolCalls[tc.Index] = state
			}
			if tc.ID != "" {
				state.id = tc.ID
			}
			if tc.Function.Name != "" {
				state.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				state.arguments.WriteString(tc.Function.Arguments)
			}
		}
	}

	maxIdx := -1
	for idx := range toolCalls {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	for i := 0; i <= maxIdx; i++ {
		state, ok := toolCalls[i]
		if ok && state != nil && onToolCall != nil {
			if err := onToolCall(orchestrator.ToolCallEventData{
				Name:      state.name,
				Arguments: state.arguments.String(),
				CallID:    state.id,
			}); err != nil {
				return "", err
			}
		}
	}

	return fullContent.String(), nil
}
