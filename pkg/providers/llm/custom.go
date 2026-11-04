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

// CustomLLM talks to a customer's own OpenAI-compatible chat completions endpoint: a LangGraph or LangServe
// deployment, a gateway such as LiteLLM, vLLM, an agent of their own on any cloud. Lokutor keeps the speech
// side (recognition, turn-taking, synthesis, tool execution) and the endpoint supplies the intelligence.
//
// What the endpoint receives is the plain OpenAI request: model (when one is set), messages, stream, and
// tools when the agent has any and tools are not switched off. What it may answer with: a server-sent-event
// stream of chat.completion.chunk (content deltas, tool_call deltas, an optional final usage chunk), or, for
// a server that ignores "stream", one chat.completion JSON body. No reply cutting or provider-specific
// request fields: those belong to the named providers.
//
// The HTTP client is the caller's, so the host decides what the endpoint may reach (Lokutor's refuses any
// non-public address); this package only speaks the protocol.
type CustomLLM struct {
	url     string
	apiKey  string
	model   string
	headers map[string]string
	client  *http.Client
	noTools bool
}

// NewCustomLLM builds the provider. baseURL is the chat completions URL or the API base ("https://host/v1" gets
// "/chat/completions" appended). headers are sent on every request (X-Api-Key, a tenant id ...); apiKey, when
// set, goes as a bearer token unless headers already carry an Authorization. noTools never sends the tools
// parameter: the endpoint runs its own tools and only talks back.
func NewCustomLLM(baseURL, apiKey, model string, headers map[string]string, client *http.Client, noTools bool) *CustomLLM {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasSuffix(u, "/chat/completions") {
		u += "/chat/completions"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &CustomLLM{url: u, apiKey: apiKey, model: model, headers: headers, client: client, noTools: noTools}
}

func (l *CustomLLM) Name() string { return "custom-llm" }

func (l *CustomLLM) payload(messages []orchestrator.Message, tools []orchestrator.Tool, stream, usage bool) map[string]interface{} {
	p := map[string]interface{}{"messages": messages}
	if l.model != "" {
		p["model"] = l.model
	}
	if stream {
		p["stream"] = true
		if usage {
			p["stream_options"] = map[string]interface{}{"include_usage": true}
		}
	}
	if len(tools) > 0 && !l.noTools {
		p["tools"] = tools
		p["tool_choice"] = "auto"
	}
	return p
}

func (l *CustomLLM) do(ctx context.Context, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", "Lokutor-CustomLLM/1.0")
	hasAuth := false
	for k, v := range l.headers {
		req.Header.Set(k, v)
		if strings.EqualFold(k, "authorization") {
			hasAuth = true
		}
	}
	if l.apiKey != "" && !hasAuth {
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &customHTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return resp, nil
}

type customHTTPError struct {
	Status int
	Body   string
}

func (e *customHTTPError) Error() string {
	return fmt.Sprintf("custom llm endpoint answered HTTP %d: %s", e.Status, e.Body)
}

// doWithUsageFallback asks for streamed usage and, when a strict server rejects the unknown parameter,
// asks again without it.
func (l *CustomLLM) doStream(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (*http.Response, error) {
	resp, err := l.do(ctx, l.payload(messages, tools, true, true))
	if he, ok := err.(*customHTTPError); ok && (he.Status == 400 || he.Status == 422) && strings.Contains(strings.ToLower(he.Body), "stream_options") {
		return l.do(ctx, l.payload(messages, tools, true, false))
	}
	return resp, err
}

type customChoice struct {
	Delta   *customMessage `json:"delta"`
	Message *customMessage `json:"message"`
}

type customMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (l *CustomLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	resp, err := l.do(ctx, l.payload(messages, tools, false, false))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []customChoice `json:"choices"`
		Usage   usagePayload   `json:"usage"`
		Error   interface{}    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("custom llm endpoint error: %v", out.Error)
	}
	recordUsage(ctx, out.Usage)
	if len(out.Choices) == 0 || out.Choices[0].Message == nil {
		return "", fmt.Errorf("custom llm endpoint returned no message")
	}
	m := out.Choices[0].Message
	calls := make([]openAIToolCall, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		var c openAIToolCall
		c.ID = tc.ID
		c.Function.Name = tc.Function.Name
		c.Function.Arguments = tc.Function.Arguments
		calls = append(calls, c)
	}
	captureToolCalls(ctx, calls)
	return m.Content, nil
}

func (l *CustomLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	resp, err := l.doStream(ctx, messages, tools)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	type callState struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*callState{}
	var full strings.Builder

	emit := func(m *customMessage) error {
		if m == nil {
			return nil
		}
		if m.Content != "" {
			full.WriteString(m.Content)
			if onChunk != nil {
				if err := onChunk(m.Content); err != nil {
					return err
				}
			}
		}
		for _, tc := range m.ToolCalls {
			st := calls[tc.Index]
			if st == nil {
				st = &callState{}
				calls[tc.Index] = st
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			// Arguments arrive as a string in OpenAI's format; a server that sends an object is accepted too.
			if a := tc.Function.Arguments; len(a) > 0 && string(a) != "null" {
				var s string
				if json.Unmarshal(a, &s) == nil {
					st.args.WriteString(s)
				} else {
					st.args.Write(a)
				}
			}
		}
		return nil
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		// A server that ignores "stream": one chat.completion body.
		var out struct {
			Choices []customChoice `json:"choices"`
			Usage   usagePayload   `json:"usage"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
			return "", err
		}
		recordUsage(ctx, out.Usage)
		if len(out.Choices) > 0 {
			if err := emit(out.Choices[0].Message); err != nil {
				return "", err
			}
		}
	} else {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return full.String(), err
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					break
				}
				var chunk struct {
					Error   interface{}    `json:"error"`
					Choices []customChoice `json:"choices"`
				}
				recordUsageFromChunk(ctx, []byte(data))
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if chunk.Error != nil {
						return full.String(), fmt.Errorf("custom llm endpoint stream error: %v", chunk.Error)
					}
					if len(chunk.Choices) > 0 {
						if e := emit(chunk.Choices[0].Delta); e != nil {
							return "", e
						}
					}
				}
			}
			if err == io.EOF {
				break
			}
		}
	}

	maxIdx := -1
	for i := range calls {
		if i > maxIdx {
			maxIdx = i
		}
	}
	for i := 0; i <= maxIdx; i++ {
		if st := calls[i]; st != nil && st.name != "" && onToolCall != nil {
			if err := onToolCall(orchestrator.ToolCallEventData{Name: st.name, Arguments: st.args.String(), CallID: st.id}); err != nil {
				return "", err
			}
		}
	}
	return full.String(), nil
}
