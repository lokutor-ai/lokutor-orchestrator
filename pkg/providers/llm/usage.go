package llm

import (
	"context"
	"encoding/json"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// Token-usage plumbing shared by the OpenAI-compatible providers (Groq, Cerebras, OpenAI).
//
// These APIs report token counts in a `usage` object. On a NON-streaming call it sits on the
// response body. On a STREAMING call it is omitted by default and only sent — in a final chunk
// whose `choices` array is empty — when the request asks for it via `stream_options`. That empty
// choices array is why the usage chunk was invisible: every streaming parser here bails out with
// `if len(chunk.Choices) == 0 { continue }` before looking at anything else, so the one chunk
// carrying the numbers is precisely the one that got skipped.

// usagePayload is the `usage` object as these APIs return it.
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// requestStreamUsage asks an OpenAI-compatible endpoint to append a usage chunk to the stream.
// Harmless where unsupported: an unknown field in the body is ignored, and the parser treats a
// missing usage chunk as "not reported" rather than zero.
func requestStreamUsage(payload map[string]interface{}) {
	payload["stream_options"] = map[string]interface{}{"include_usage": true}
}

// recordUsage writes counts into the context's sink, if the caller installed one.
func recordUsage(ctx context.Context, u usagePayload) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return
	}
	orchestrator.TokenUsageFrom(ctx).Record(u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	// And into the conversation's bill (orchestrator.SessionMeter), under the category the call site set.
	orchestrator.SessionMeterFrom(ctx).Add(orchestrator.TokenCategoryFrom(ctx), u.PromptTokens, u.CompletionTokens)
}

// recordUsageFromChunk pulls `usage` out of one streaming chunk. Call it for EVERY chunk, before
// any choices-based early return.
func recordUsageFromChunk(ctx context.Context, data []byte) {
	if orchestrator.TokenUsageFrom(ctx) == nil && orchestrator.SessionMeterFrom(ctx) == nil {
		return
	}
	var envelope struct {
		Usage *usagePayload `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Usage == nil {
		return
	}
	recordUsage(ctx, *envelope.Usage)
}

// openAIToolCall is one tool call of a non-streaming OpenAI-format reply (choices[0].message.tool_calls).
// Arguments is kept raw: it is a JSON string in the format, and an object from a provider that differs must not fail
// the whole decode, as a field Complete never read until 2026-10-16 could not.
type openAIToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// captureToolCalls hands a Complete reply's tool calls to the caller's capture (orchestrator.WithToolCallCapture),
// if it installed one: Complete returns only the text.
func captureToolCalls(ctx context.Context, calls []openAIToolCall) {
	c := orchestrator.ToolCallCaptureFrom(ctx)
	if c == nil {
		return
	}
	for _, tc := range calls {
		args := string(tc.Function.Arguments)
		var s string
		if json.Unmarshal(tc.Function.Arguments, &s) == nil {
			args = s
		}
		c.Add(orchestrator.ToolCallEventData{Name: tc.Function.Name, Arguments: args, CallID: tc.ID})
	}
}
