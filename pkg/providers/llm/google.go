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
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type GoogleLLM struct {
	apiKey          string
	completeURL     string
	streamURL       string
	model           string
}

func NewGoogleLLM(apiKey string, model string) *GoogleLLM {
	if model == "" {
		model = "gemini-2.5-flash"
	}
	base := "https://generativelanguage.googleapis.com/v1beta/models/" + model
	return &GoogleLLM{
		apiKey:      apiKey,
		completeURL: base + ":generateContent",
		streamURL:   base + ":streamGenerateContent?alt=sse",
		model:       model,
	}
}

type googlePart struct {
	Text             string      `json:"text,omitempty"`
	FunctionCall     interface{} `json:"functionCall,omitempty"`
	FunctionResponse interface{} `json:"functionResponse,omitempty"`
}

type googleContent struct {
	Role  string        `json:"role"`
	Parts []googlePart `json:"parts"`
}

type googleTool struct {
	FunctionDeclarations []interface{} `json:"functionDeclarations"`
}

func (l *GoogleLLM) buildRequest(messages []orchestrator.Message, tools []orchestrator.Tool) map[string]interface{} {
	var systemInstruction *googleContent
	var contents []googleContent

	// Track function names by call ID so tool results can reference them
	fnNameByCallID := make(map[string]string)

	for _, m := range messages {
		if m.Role == "system" {
			if systemInstruction == nil {
				systemInstruction = &googleContent{
					Role:  "user",
					Parts: []googlePart{{Text: m.Content}},
				}
			} else {
				systemInstruction.Parts[0].Text += "\n" + m.Content
			}
			continue
		}

		if m.Role == "assistant" && m.ToolCalls != nil {
			// Build functionCall parts from the stored tool call data
			tcList, ok := m.ToolCalls.([]interface{})
			if !ok {
				continue
			}
			var parts []googlePart
			if m.Content != "" {
				parts = append(parts, googlePart{Text: m.Content})
			}
			for _, tc := range tcList {
				tcMap, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := tcMap["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				argsStr, _ := fn["arguments"].(string)
				var args interface{}
				json.Unmarshal([]byte(argsStr), &args)
				if name == "" {
					continue
				}
				callID, _ := tcMap["id"].(string)
				if callID != "" {
					fnNameByCallID[callID] = name
				}
				parts = append(parts, googlePart{
					FunctionCall: map[string]interface{}{
						"name": name,
						"args": args,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, googleContent{
					Role:  "model",
					Parts: parts,
				})
			}
			continue
		}

		if m.Role == "tool" {
			// Look up function name by call ID
			fnName := fnNameByCallID[m.ToolCallID]
			if fnName == "" {
				// Fallback: use the Name field if set
				fnName = m.Name
			}
			if fnName == "" {
				continue
			}
			contents = append(contents, googleContent{
				Role: "function",
				Parts: []googlePart{{
					FunctionResponse: map[string]interface{}{
						"name": fnName,
						"response": map[string]string{
							"response": m.Content,
						},
					},
				}},
			})
			continue
		}

		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, googleContent{
			Role:  role,
			Parts: []googlePart{{Text: m.Content}},
		})
	}

	// Gemini requires contents to always end with a user role.
	// If the last message is from model, drop it — this happens on silence timeout
	// or first-speaker greeting where the session ends with an assistant response.
	for len(contents) > 0 && contents[len(contents)-1].Role == "model" {
		contents = contents[:len(contents)-1]
	}
	// Contents must not be empty — Gemini rejects empty contents.
	if len(contents) == 0 {
		contents = append(contents, googleContent{Role: "user", Parts: []googlePart{{Text: "Hello"}}})
	}

	payload := map[string]interface{}{
		"contents": contents,
	}

	if systemInstruction != nil {
		payload["system_instruction"] = systemInstruction
	}

	if len(tools) > 0 {
		var funcDecls []interface{}
		for _, t := range tools {
			funcDecls = append(funcDecls, t.Function)
		}
		payload["tools"] = []googleTool{{FunctionDeclarations: funcDecls}}
	}

	return payload
}

func (l *GoogleLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	payload := l.buildRequest(messages, tools)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", l.completeURL+"?key="+l.apiKey, bytes.NewReader(body))
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
					Text         string      `json:"text"`
					FunctionCall interface{} `json:"functionCall"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no response from google llm")
	}

	// If the response contains a functionCall, return the text if any, or empty
	for _, part := range result.Candidates[0].Content.Parts {
		if part.FunctionCall != nil {
			return result.Candidates[0].Content.Parts[0].Text, nil
		}
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}

func (l *GoogleLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	payload := l.buildRequest(messages, tools)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", l.streamURL+"&key="+l.apiKey, bytes.NewReader(body))
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
		return "", fmt.Errorf("google llm stream error (status %d): %v", resp.StatusCode, errResp)
	}

	reader := bufio.NewReader(resp.Body)
	var fullContent strings.Builder
	var prevText string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		raw := strings.TrimPrefix(line, "data: ")
		if raw == "" {
			continue
		}

		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string      `json:"text"`
						FunctionCall interface{} `json:"functionCall"`
					} `json:"parts"`
					Role string `json:"role"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			continue
		}

		if len(chunk.Candidates) == 0 || len(chunk.Candidates[0].Content.Parts) == 0 {
			continue
		}

		// Check for functionCall in any part
		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				fc, ok := part.FunctionCall.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := fc["name"].(string)
				if name == "" {
					continue
				}
				argsBytes, _ := json.Marshal(fc["args"])
				callID := fmt.Sprintf("fc_%s_%d", name, time.Now().UnixNano())
				if onToolCall != nil {
					if err := onToolCall(orchestrator.ToolCallEventData{
						Name:      name,
						Arguments: string(argsBytes),
						CallID:    callID,
					}); err != nil {
						return "", err
					}
				}
				// Function call was handled, no text content to stream
				return "", nil
			}
		}

		t := chunk.Candidates[0].Content.Parts[0].Text

		var delta string
		if len(t) > len(prevText) && strings.HasPrefix(t, prevText) {
			delta = t[len(prevText):]
			prevText = t
		} else if len(t) > 0 && !strings.HasPrefix(t, prevText) {
			delta = t
			prevText = t
		}
		if delta == "" {
			continue
		}
		fullContent.WriteString(delta)
		if onChunk != nil {
			if err := onChunk(delta); err != nil {
				return "", err
			}
		}
	}

	return fullContent.String(), nil
}

func (l *GoogleLLM) Name() string {
	return "google-llm"
}
