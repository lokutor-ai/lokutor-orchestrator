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

type GroqLLM struct {
	apiKey string
	url    string
	model  string
	// reasoningEffort is sent as "reasoning_effort" when non-empty. See
	// applyReasoningEffort for why this is the single biggest latency lever
	// on the current production model.
	reasoningEffort string
	// toolHistory is where a gpt-oss model is sent earlier tool exchanges; see forGptOss.
	toolHistory GptOssToolHistory
}

func NewGroqLLM(apiKey string, model string) *GroqLLM {
	if model == "" {
		model = "meta-llama/llama-4-scout-17b-16e-instruct"
	}
	return &GroqLLM{
		apiKey:          apiKey,
		url:             "https://api.groq.com/openai/v1/chat/completions",
		model:           model,
		reasoningEffort: defaultReasoningEffort(model),
	}
}

func (l *GroqLLM) applyReasoningEffort(ctx context.Context, payload map[string]interface{}) {
	setReasoningEffort(payload, effortFor(ctx, l.reasoningEffort))
}

func (l *GroqLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	payload := map[string]interface{}{
		"model":    l.model,
		"messages": forGptOss(l.model, messages, l.toolHistory),
	}
	l.applyReasoningEffort(ctx, payload)
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

	resp, err := getSharedLLMClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("groq api error (status %d): %v", resp.StatusCode, errResp)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage usagePayload `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	recordUsage(ctx, result.Usage)

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from groq")
	}

	return result.Choices[0].Message.Content, nil
}

func (l *GroqLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	payload := map[string]interface{}{
		"model":    l.model,
		"messages": forGptOss(l.model, messages, l.toolHistory),
		"stream":   true,
	}
	// Streaming omits token counts unless asked; see requestStreamUsage in usage.go.
	requestStreamUsage(payload)
	l.applyReasoningEffort(ctx, payload)
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

	resp, err := getSharedLLMClient().Do(req)
	if err != nil {
		return "", err
	}
	// Closed here unless a cut reply hands the rest of the stream to drainAfterCut.
	drained := false
	defer func() {
		if !drained {
			resp.Body.Close()
		}
	}()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("groq api error (status %d): %v", resp.StatusCode, errResp)
	}

	reader := bufio.NewReader(resp.Body)
	var fullContent strings.Builder
	turn := oneTurnFor(ctx, l.model)

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
					Reasoning string `json:"reasoning"`
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

		// Before the choices check, not after: the chunk carrying `usage` has an EMPTY
		// choices array, so the early return below is exactly what hid it.
		recordUsageFromChunk(ctx, []byte(data))

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if text := turn.Chunk(delta.Reasoning, delta.Content); text != "" {
			fullContent.WriteString(text)
			if onChunk != nil {
				if err := onChunk(text); err != nil {
					return "", err
				}
			}
		}
		if turn.Ended() {
			drained = true
			drainAfterCut(ctx, reader, resp.Body, turn.Cut())
			break
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

	// Emit tool calls if any - iterating safely over max observed index
	maxIdx := -1
	for idx := range toolCalls {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	for i := 0; i <= maxIdx; i++ {
		state, ok := toolCalls[i]
		if ok && state != nil && onToolCall != nil {
			err := onToolCall(orchestrator.ToolCallEventData{
				Name:      state.name,
				Arguments: state.arguments.String(),
				CallID:    state.id,
			})
			if err != nil {
				return "", err
			}
		}
	}

	return fullContent.String(), nil
}

func (l *GroqLLM) Name() string {
	return "groq-llm"
}
