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
			// Parse response as JSON object if possible, otherwise wrap in result object.
			// Gemini requires functionResponse.response to be a JSON object (protobuf Struct),
			// not a string, number, array, or null.
			var respObj interface{}
			if json.Unmarshal([]byte(m.Content), &respObj) == nil {
				if obj, ok := respObj.(map[string]interface{}); ok {
					contents = append(contents, googleContent{
						Role: "user",
						Parts: []googlePart{{
							FunctionResponse: map[string]interface{}{
								"name": fnName,
								"response": obj,
							},
						}},
					})
				} else {
					contents = append(contents, googleContent{
						Role: "user",
						Parts: []googlePart{{
							FunctionResponse: map[string]interface{}{
								"name": fnName,
								"response": map[string]interface{}{"result": m.Content},
							},
						}},
					})
				}
			} else {
				contents = append(contents, googleContent{
					Role: "user",
					Parts: []googlePart{{
						FunctionResponse: map[string]interface{}{
							"name": fnName,
							"response": map[string]interface{}{"result": m.Content},
						},
					}},
				})
			}
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

// If the model generated a function call, serialize it as JSON so the
	// caller can dispatch it (the orchestrator uses StreamComplete for full
	// tool calling, but Complete() should not silently drop the call).
	for _, part := range result.Candidates[0].Content.Parts {
		if part.FunctionCall != nil {
			b, err := json.Marshal(part.FunctionCall)
			if err == nil {
				return fmt.Sprintf(`[TOOL_CALL] %s`, string(b)), nil
			}
		}
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}


func (l *GoogleLLM) Name() string {
	return "google-llm"
}

// StreamComplete implements StreamingLLMProvider for Gemini. It streams text
// chunks via SSE and invokes onToolCall for any functionCall parts.
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
		return "", fmt.Errorf("google llm error (status %d): %v", resp.StatusCode, errResp)
	}

	reader := bufio.NewReader(resp.Body)
	var fullContent strings.Builder
	toolCallIndex := 0

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Gemini SSE format: data: {...}\n
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall struct {
							Name string      `json:"name"`
							Args interface{} `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Candidates) == 0 || len(chunk.Candidates[0].Content.Parts) == 0 {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.Text != "" {
				fullContent.WriteString(part.Text)
				if onChunk != nil {
					if err := onChunk(part.Text); err != nil {
						return "", err
					}
				}
			}
			if part.FunctionCall.Name != "" {
				if onToolCall != nil {
					argsJSON, _ := json.Marshal(part.FunctionCall.Args)
					// Gemini doesn't provide call IDs. Using the bare name
					// collided when the model called the same tool twice in
					// one turn (e.g. two search_knowledge_base calls with
					// different queries): both got the same CallID, results
					// are correlated by CallID, so the second silently
					// clobbered the first's pending result and one call
					// always timed out. Suffix with a per-response index to
					// keep concurrent calls to the same tool distinct.
					callID := fmt.Sprintf("%s_%d", part.FunctionCall.Name, toolCallIndex)
					toolCallIndex++
					err := onToolCall(orchestrator.ToolCallEventData{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
						CallID:    callID,
					})
					if err != nil {
						return "", err
					}
				}
			}
		}
	}

	return fullContent.String(), nil
}
