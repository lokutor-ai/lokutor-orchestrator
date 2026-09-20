package llm

import (
	"context"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// The usage chunk arrives with an EMPTY choices array, which is the whole difficulty: every
// streaming parser here does `if len(chunk.Choices) == 0 { continue }`, so the one chunk carrying
// the token counts is precisely the one that gets skipped. recordUsageFromChunk must therefore run
// BEFORE that check, and this test pins the shape that makes it necessary.
func TestUsageIsReadFromAChunkWithNoChoices(t *testing.T) {
	sink := &orchestrator.TokenUsage{}
	ctx := orchestrator.WithTokenUsage(context.Background(), sink)

	recordUsageFromChunk(ctx, []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":48,"total_tokens":1248}}`))

	p, c, total, ok := sink.Snapshot()
	if !ok {
		t.Fatal("no usage recorded from a chunk whose choices array is empty — this is the exact shape the parsers skip")
	}
	if p != 1200 || c != 48 || total != 1248 {
		t.Errorf("got prompt=%d completion=%d total=%d, want 1200/48/1248", p, c, total)
	}
}

// A turn can make several provider calls — a tool round-trip is two — and the turn's cost is their
// sum. Overwriting would under-report exactly the turns that cost the most.
func TestUsageAccumulatesAcrossCallsInOneTurn(t *testing.T) {
	sink := &orchestrator.TokenUsage{}
	ctx := orchestrator.WithTokenUsage(context.Background(), sink)

	recordUsageFromChunk(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":20,"total_tokens":920}}`))
	recordUsageFromChunk(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":1100,"completion_tokens":35,"total_tokens":1135}}`))

	p, c, total, _ := sink.Snapshot()
	if p != 2000 || c != 55 || total != 2055 {
		t.Errorf("got prompt=%d completion=%d total=%d, want 2000/55/2055 (summed, not overwritten)", p, c, total)
	}
}

// "Not reported" must stay distinguishable from a genuine zero. They average very differently and
// only one of them is a reason to go looking at the provider.
func TestUnreportedUsageIsNotZero(t *testing.T) {
	sink := &orchestrator.TokenUsage{}
	ctx := orchestrator.WithTokenUsage(context.Background(), sink)

	recordUsageFromChunk(ctx, []byte(`{"choices":[{"delta":{"content":"hola"}}]}`))

	if _, _, _, ok := sink.Snapshot(); ok {
		t.Error("a content chunk with no usage object marked usage as reported")
	}
}

// Every provider call passes a context; most carry no sink. That path must cost nothing and must
// never panic, because it is the common one.
func TestNoSinkIsSafe(t *testing.T) {
	recordUsageFromChunk(context.Background(), []byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
	recordUsage(context.Background(), usagePayload{PromptTokens: 5})
	var nilSink *orchestrator.TokenUsage
	nilSink.Record(1, 2, 3) // must not panic
	if _, _, _, ok := nilSink.Snapshot(); ok {
		t.Error("nil sink reported usage")
	}
}

// The request must actually ask for usage, or streaming calls report nothing at all.
func TestStreamRequestsUsage(t *testing.T) {
	payload := map[string]interface{}{"model": "gpt-oss-120b", "stream": true}
	requestStreamUsage(payload)

	opts, ok := payload["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatal("stream_options missing — streaming would silently report no tokens")
	}
	if opts["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", opts["include_usage"])
	}
}
