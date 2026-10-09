package llm

import (
	"context"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// Every usage report a provider parses reaches the conversation's bill, under the call's category, and
// a streaming usage chunk is parsed even when only the meter (no per-turn sink) is installed.
func TestUsageReachesTheSessionMeter(t *testing.T) {
	m := orchestrator.NewSessionMeter()
	ctx := orchestrator.WithSessionMeter(context.Background(), m)
	recordUsage(ctx, usagePayload{PromptTokens: 1000, CompletionTokens: 50})
	recordUsageFromChunk(orchestrator.WithTokenCategory(ctx, orchestrator.TokensSpeculative),
		[]byte(`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":40,"total_tokens":940}}`))
	got := m.Snapshot()
	if r := got[orchestrator.TokensReply]; r.Prompt != 1000 || r.Completion != 50 {
		t.Fatalf("reply = %+v", r)
	}
	if s := got[orchestrator.TokensSpeculative]; s.Prompt != 900 || s.Completion != 40 {
		t.Fatalf("speculative = %+v", s)
	}
}
