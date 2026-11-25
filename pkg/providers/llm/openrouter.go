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

// OpenRouterLLM talks to OpenRouter's OpenAI-compatible chat completions API, pinned to the providers
// it is given. It exists as the chain's second provider -- the hedge and the overflow behind Cerebras
// -- because Groq, which held that place, is capped at 8,000 tokens a minute and 200,000 a day and
// refuses at once under any load, so the turns Cerebras refused (its own 500,000 a minute, reached
// at 28 simultaneous callers on 2026-09-29) went silent.
//
// Measured the same day against Cerebras gpt-oss-120b on production's request shape
// (lokutor_tts finance/measurements/llm_candidates_openrouter_2026-09-29.json): DeepSeek-V4.1-Flash
// on Wafer with reasoning off reached its first token at 426 ms p50 / 548 p90 against 467 / 1,042,
// at a third of the price per turn, and passed 18 of 21 tool-call scenarios against 21.
//
// Requests are pinned: no fallback to other hosts of the model (their quality differs -- one leaked
// its reasoning into the reply), and only hosts that retain no data. The hosts are tried in the order
// given (provider.order, with provider.only as the fence): until 2026-11-23 they were sent as `only`
// alone, which OpenRouter reads as an allowlist and load-balances across, so "Europe, then the US"
// sent a share of the talker's requests to the US with Europe up.
//
// On Anthropic models (Claude Haiku 5.5, the talker) requests carry prompt-cache breakpoints: see
// withPromptCache.
type OpenRouterLLM struct {
	apiKey    string
	url       string
	model     string
	providers []string
	// reasoning is "none" to turn the model's thinking off, an effort ("low", "medium", "high"), or ""
	// for the model's default.
	reasoning string
	// toolHistory is where a gpt-oss model is sent earlier tool exchanges; see forGptOss.
	toolHistory GptOssToolHistory
	// capTokens caps a reply whatever the reasoning (SetMaxTokens); 0 = none.
	capTokens int
	// defaultMaxTokens caps a reply when the caller set no cap (SetDefaultMaxTokens); 0 = none.
	defaultMaxTokens int
}

// SetDefaultMaxTokens caps every reply whose caller set no cap of its own (orchestrator.WithMaxTokens),
// under the same condition as that cap: reasoning off. Without any cap OpenRouter counts a request's
// whole possible completion against the account's balance while it runs, and refuses requests when that
// estimate passes the balance; the talker's replies are short (2026-11-23: 410 completion tokens the
// longest turn in 14 days, 173 at p99).
func (l *OpenRouterLLM) SetDefaultMaxTokens(n int) { l.defaultMaxTokens = n }

// SetMaxTokens caps every reply, thinking included, for a model that cannot have its thinking switched off. Without a
// cap OpenRouter holds the request's whole possible completion against the account's balance while it runs (64,000
// tokens of Claude Sonnet 5.5 hold $0.64), and refuses every request on the account once the holds pass the balance.
// It must leave room for the thinking: 4,096 for a spoken reply at "minimal" (1,333 thinking tokens over 24 replies,
// 2026-11-25).
func (l *OpenRouterLLM) SetMaxTokens(n int) { l.capTokens = n }

func NewOpenRouterLLM(apiKey, model string, providers []string, reasoning string) *OpenRouterLLM {
	if model == "" {
		model = "deepseek/deepseek-v4.1-flash"
	}
	return &OpenRouterLLM{
		apiKey:    apiKey,
		url:       "https://openrouter.ai/api/v1/chat/completions",
		model:     model,
		providers: providers,
		reasoning: reasoning,
	}
}

func (l *OpenRouterLLM) Name() string { return "openrouter-llm" }

func (l *OpenRouterLLM) payload(messages []orchestrator.Message, tools []orchestrator.Tool, stream bool) map[string]interface{} {
	p := map[string]interface{}{
		"model":    l.model,
		"messages": forGptOss(l.model, messages, l.toolHistory),
	}
	if stream {
		p["stream"] = true
		requestStreamUsage(p)
	}
	routing := map[string]interface{}{"zdr": true, "data_collection": "deny"}
	if len(l.providers) > 0 {
		// order: the hosts are tried first to last. only fences the request to them and
		// allow_fallbacks=false keeps out every other public host; only alone is an allowlist that
		// OpenRouter load-balances across, which is not "the first, then the next".
		routing["order"] = l.providers
		routing["only"] = l.providers
		routing["allow_fallbacks"] = false
	}
	p["provider"] = routing
	if isAnthropicModel(l.model) {
		withPromptCache(p)
	}
	if isGoogleModel(l.model) {
		p["messages"] = withThoughtSignatures(messages)
	}
	switch r := strings.ToLower(strings.TrimSpace(l.reasoning)); r {
	case "":
	case "none", "off":
		p["reasoning"] = map[string]interface{}{"enabled": false}
	default:
		p["reasoning"] = map[string]interface{}{"effort": r}
	}
	if len(tools) > 0 {
		p["tools"] = tools
		p["tool_choice"] = "auto"
	}
	return p
}

// isGoogleModel reports a Gemini model ("google/gemini-3.8-flash").
func isGoogleModel(model string) bool { return strings.HasPrefix(strings.ToLower(model), "google/") }

// withThoughtSignatures is messages with a thought signature on every message that called tools. Gemini 3 and
// later refuse a history whose tool calls carry none ("Request contains an invalid argument", every turn after a
// tool call; 2026-11-25). The signature is the model's own, encrypted, and the conversation's history keeps none of
// it, nor can it when the call was made by another model earlier on the same call (a flow's step before this one).
// Google documents this value for both cases: the validator is skipped. Measured from a worker pod that day: 12 of
// 12 turns right after it, as with the real ones (lokutor_tts finance/measurements/agent_models_2026-11-25).
func withThoughtSignatures(messages []orchestrator.Message) []interface{} {
	out := make([]interface{}, 0, len(messages))
	for _, m := range messages {
		id := firstToolCallID(m)
		if m.Role != "assistant" || id == "" {
			out = append(out, m)
			continue
		}
		out = append(out, map[string]interface{}{"role": m.Role, "content": m.Content, "tool_calls": m.ToolCalls,
			"reasoning_details": []interface{}{map[string]interface{}{"type": "reasoning.encrypted",
				"data": "skip_thought_signature_validator", "id": id, "format": "google-gemini-v1", "index": 0}}})
	}
	return out
}

// firstToolCallID is the id of the first tool call a message made, or "".
func firstToolCallID(m orchestrator.Message) string {
	if m.ToolCalls == nil {
		return ""
	}
	b, err := json.Marshal(m.ToolCalls)
	if err != nil {
		return ""
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &calls) != nil || len(calls) == 0 {
		return ""
	}
	return calls[0].ID
}

// withMaxTokens adds the cap a caller set with orchestrator.WithMaxTokens, only when this model does not reason
// first (reasoning explicitly off, as the natural talker runs): a model that may reason would spend the cap thinking.
func (l *OpenRouterLLM) withMaxTokens(ctx context.Context, p map[string]interface{}) map[string]interface{} {
	if l.capTokens > 0 {
		p["max_tokens"] = l.capTokens
	}
	if t, ok := orchestrator.TemperatureFrom(ctx); ok {
		p["temperature"] = t
	}
	n := orchestrator.MaxTokensFrom(ctx)
	if n <= 0 {
		n = l.defaultMaxTokens
	}
	if n <= 0 {
		return p
	}
	switch strings.ToLower(strings.TrimSpace(l.reasoning)) {
	case "none", "off":
		p["max_tokens"] = n
	}
	return p
}

// request is the payload of one request, as its caller asked for it.
func (l *OpenRouterLLM) request(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, stream bool) map[string]interface{} {
	p := l.payload(messages, tools, stream)
	if isAnthropicModel(l.model) && orchestrator.SentOnce(ctx) {
		p["messages"] = forGptOss(l.model, messages, l.toolHistory)
		delete(p, "cache_control")
	}
	return l.withMaxTokens(ctx, p)
}

func (l *OpenRouterLLM) do(ctx context.Context, payload map[string]interface{}) (*http.Response, *sentRequest, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", l.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.apiKey)
	req.Header.Set("X-Title", "Lokutor")
	sent := startRequest(ctx, l.model, body)
	resp, err := getSharedLLMClient().Do(sent.trace(req))
	if err != nil {
		sent.end()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		sent.failed = true
		sent.end()
		defer resp.Body.Close()
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, nil, fmt.Errorf("openrouter api error (status %d): %v", resp.StatusCode, errResp)
	}
	sent.answered = true
	return resp, sent, nil
}

func (l *OpenRouterLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	resp, sent, err := l.do(ctx, l.request(ctx, messages, tools, false))
	if err != nil {
		return "", err
	}
	defer sent.end()
	defer resp.Body.Close()
	var result struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage    usagePayload `json:"usage"`
		Provider string       `json:"provider"`
		Error    interface{}  `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != nil {
		sent.failed = true
		return "", fmt.Errorf("openrouter api error: %v", result.Error)
	}
	orchestrator.TokenUsageFrom(ctx).SetUpstream(result.Provider)
	sent.usage(result.Usage)
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from openrouter")
	}
	captureToolCalls(ctx, result.Choices[0].Message.ToolCalls)
	return result.Choices[0].Message.Content, nil
}

func (l *OpenRouterLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	resp, sent, err := l.do(ctx, l.request(ctx, messages, tools, true))
	if err != nil {
		return "", err
	}
	// Closed here unless a cut reply hands the rest of the stream to drainAfterCut.
	drained := false
	defer func() {
		if !drained {
			resp.Body.Close()
			sent.end()
		}
	}()

	reader := bufio.NewReader(resp.Body)
	var fullContent strings.Builder
	turn := oneTurnFor(ctx, l.model)
	type toolCallState struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := make(map[int]*toolCallState)
	upstreamSeen := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			sent.broke()
			return "", err
		}
		line = strings.TrimSpace(line)
		// OpenRouter sends ": OPENROUTER PROCESSING" comments while it waits on the provider.
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Error    interface{} `json:"error"`
			Provider string      `json:"provider"`
			Choices  []struct {
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
		sent.chunk([]byte(data))
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if !upstreamSeen && chunk.Provider != "" {
			upstreamSeen = true
			orchestrator.TokenUsageFrom(ctx).SetUpstream(chunk.Provider)
		}
		// A provider failing after OpenRouter has answered 200 arrives as an error chunk. Before any
		// output it is an ordinary failure, so the chain moves on; after output it ends the turn.
		if chunk.Error != nil {
			sent.failed = true
			return fullContent.String(), fmt.Errorf("openrouter stream error: %v", chunk.Error)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if text := turn.Chunk(delta.Reasoning, delta.Content); text != "" {
			fullContent.WriteString(text)
			if onChunk != nil {
				if err := onChunk(text); err != nil {
					sent.stopped = true
					return "", err
				}
			}
		}
		if turn.Ended() {
			drained = true
			drainAfterCut(ctx, reader, resp.Body, turn.Cut(), sent)
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

// isAnthropicModel reports whether an OpenRouter model id is one of Anthropic's ("anthropic/claude-...").
func isAnthropicModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/")
}

// withPromptCache marks an Anthropic request's prompt for Anthropic's prompt cache, which OpenRouter
// passes to Vertex and Bedrock as it is. Without marks nothing is cached: these hosts have no implicit
// caching (OpenRouter's endpoints list, supports_implicit_caching false), and every request paid its
// whole prompt (4,400 tokens p50, 12,100 p90 per turn, 2026-11-23) at full price.
//
// A cache read costs a tenth of the input price, a write 1.25 times it, and the cache matches whole
// prefixes (tools, then system, then messages), up to four marks a request. Three marks:
//   - the end of the first system message: the agent's prompt, fixed for a call once its parts are in
//     (lokutor_tts call_context.go puts the expression-tags section last for this), and shared by
//     every request of the call;
//   - the last message before the caller's latest turn: the history every request of this turn
//     shares, the speculative ones, the real one and the tool rounds after it;
//   - the request's end (the top-level mark, which the host places on the last block), so the next
//     request -- the real reply after a speculative one, a tool round, the next turn -- reads all of
//     this one.
//
// Anything after a mark that changes (a knowledge passage after the caller's words, a call summary
// replacing turns) costs only the part after the last unchanged mark. Prefixes under the model's
// minimum (512 tokens on Claude Haiku 5.5) are silently not cached, and cost the same as unmarked.
//
// The customer's bill does not change: tokens are billed at the rate card on the whole prompt; the
// cache lowers what the requests cost us.
func withPromptCache(p map[string]interface{}) {
	msgs, ok := p["messages"].([]orchestrator.Message)
	if !ok || len(msgs) == 0 {
		return
	}
	marks := map[int]bool{}
	for i, m := range msgs {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			marks[i] = true
			break
		}
	}
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}
	for i := lastUser - 1; i > 0; i-- {
		m := msgs[i]
		if (m.Role == "user" || m.Role == "assistant") && m.ToolCalls == nil && strings.TrimSpace(m.Content) != "" {
			marks[i] = true
			break
		}
	}
	out := make([]interface{}, len(msgs))
	for i, m := range msgs {
		if !marks[i] {
			out[i] = m
			continue
		}
		mm := map[string]interface{}{
			"role": m.Role,
			"content": []map[string]interface{}{{
				"type": "text", "text": m.Content, "cache_control": map[string]interface{}{"type": "ephemeral"},
			}},
		}
		if m.Name != "" {
			mm["name"] = m.Name
		}
		out[i] = mm
	}
	p["messages"] = out
	p["cache_control"] = map[string]interface{}{"type": "ephemeral"}
}
