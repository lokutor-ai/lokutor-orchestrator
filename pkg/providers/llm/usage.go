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
}

// recordUsageFromChunk pulls `usage` out of one streaming chunk. Call it for EVERY chunk, before
// any choices-based early return.
func recordUsageFromChunk(ctx context.Context, data []byte) {
	if orchestrator.TokenUsageFrom(ctx) == nil {
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
