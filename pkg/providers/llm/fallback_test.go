package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// mockStreamingLLM is a minimal orchestrator.StreamingLLMProvider for testing.
type mockStreamingLLM struct {
	name       string
	err        error
	chunks     []string
	toolCalls  []orchestrator.ToolCallEventData
	calls      int
	streamOnly bool // if true, Complete() is never expected to be called
}

func (m *mockStreamingLLM) Name() string { return m.name }

func (m *mockStreamingLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	m.calls++
	if m.streamOnly {
		return "", fmt.Errorf("Complete() should not have been called on %s", m.name)
	}
	if m.err != nil {
		return "", m.err
	}
	return "response from " + m.name, nil
}

func (m *mockStreamingLLM) StreamComplete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool, onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	m.calls++
	for _, c := range m.chunks {
		if err := onChunk(c); err != nil {
			return "", err
		}
	}
	for _, tc := range m.toolCalls {
		if err := onToolCall(tc); err != nil {
			return "", err
		}
	}
	if m.err != nil {
		return "", m.err
	}
	return "response from " + m.name, nil
}

func TestFallbackLLM_Complete_FailsOverOnRateLimit(t *testing.T) {
	primary := &mockStreamingLLM{name: "primary", err: fmt.Errorf("groq api error (status 429): rate limited")}
	secondary := &mockStreamingLLM{name: "secondary"}
	f := NewChainLLM("test", primary, secondary)

	text, err := f.Complete(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if text != "response from secondary" {
		t.Errorf("expected response from secondary, got %q", text)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("expected exactly one call to each provider, got primary=%d secondary=%d", primary.calls, secondary.calls)
	}
}

func TestFallbackLLM_Complete_DoesNotFailOverOnOtherErrors(t *testing.T) {
	wantErr := errors.New("groq api error (status 400): bad request")
	primary := &mockStreamingLLM{name: "primary", err: wantErr}
	secondary := &mockStreamingLLM{name: "secondary"}
	f := NewChainLLM("test", primary, secondary)

	_, err := f.Complete(context.Background(), nil, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the primary's non-rate-limit error to surface unchanged, got: %v", err)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should never be called for a non-rate-limit error, got %d calls", secondary.calls)
	}
}

func TestFallbackLLM_StreamComplete_FailsOverBeforeAnyOutput(t *testing.T) {
	primary := &mockStreamingLLM{name: "primary", err: fmt.Errorf("gemini llm error (status 429): RESOURCE_EXHAUSTED")}
	secondary := &mockStreamingLLM{name: "secondary", chunks: []string{"hello"}}
	f := NewChainLLM("test", primary, secondary)

	var gotChunks []string
	text, err := f.StreamComplete(context.Background(), nil, nil,
		func(s string) error { gotChunks = append(gotChunks, s); return nil },
		func(orchestrator.ToolCallEventData) error { return nil },
	)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if text != "response from secondary" || len(gotChunks) != 1 || gotChunks[0] != "hello" {
		t.Errorf("expected secondary's output, got text=%q chunks=%v", text, gotChunks)
	}
}

func TestFallbackLLM_StreamComplete_NoFailoverAfterPartialOutput(t *testing.T) {
	// Primary emits a chunk THEN fails with a rate-limit-shaped error — this
	// must NOT fail over, since the caller has already spoken/acted on
	// primary's partial output; retrying on secondary would duplicate it.
	primary := &mockStreamingLLM{
		name:   "primary",
		chunks: []string{"partial "},
		err:    fmt.Errorf("groq api error (status 429): rate limited mid-stream"),
	}
	secondary := &mockStreamingLLM{name: "secondary"}
	f := NewChainLLM("test", primary, secondary)

	_, err := f.StreamComplete(context.Background(), nil, nil,
		func(string) error { return nil },
		func(orchestrator.ToolCallEventData) error { return nil },
	)
	if err == nil {
		t.Fatal("expected the primary's error to surface, got nil")
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should not be called once the primary has already produced output, got %d calls", secondary.calls)
	}
}

func TestFallbackLLM_StreamComplete_NoFailoverAfterToolCall(t *testing.T) {
	primary := &mockStreamingLLM{
		name:      "primary",
		toolCalls: []orchestrator.ToolCallEventData{{Name: "get_weather", CallID: "c1"}},
		err:       fmt.Errorf("groq api error (status 429): rate limited after tool call"),
	}
	secondary := &mockStreamingLLM{name: "secondary"}
	f := NewChainLLM("test", primary, secondary)

	var gotCalls int
	_, err := f.StreamComplete(context.Background(), nil, nil,
		func(string) error { return nil },
		func(orchestrator.ToolCallEventData) error { gotCalls++; return nil },
	)
	if err == nil {
		t.Fatal("expected the primary's error to surface, got nil")
	}
	if gotCalls != 1 {
		t.Errorf("expected exactly one tool call to have been dispatched, got %d", gotCalls)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should not be called once the primary has already dispatched a tool call, got %d calls", secondary.calls)
	}
}

func TestChainLLM_ThreeProviders_FailsOverTwice(t *testing.T) {
	first := &mockStreamingLLM{name: "first", err: fmt.Errorf("cerebras api error (status 429): rate limited")}
	second := &mockStreamingLLM{name: "second", err: fmt.Errorf("groq api error (status 429): rate limited")}
	third := &mockStreamingLLM{name: "third", chunks: []string{"ok"}}
	c := NewChainLLM("test", first, second, third)

	text, err := c.StreamComplete(context.Background(), nil, nil,
		func(string) error { return nil },
		func(orchestrator.ToolCallEventData) error { return nil },
	)
	if err != nil {
		t.Fatalf("expected the chain to reach the third provider, got error: %v", err)
	}
	if text != "response from third" {
		t.Errorf("expected response from third, got %q", text)
	}
	if first.calls != 1 || second.calls != 1 || third.calls != 1 {
		t.Errorf("expected exactly one call to each provider, got first=%d second=%d third=%d", first.calls, second.calls, third.calls)
	}
}

func TestFallbackLLM_StreamComplete_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	primary := &mockStreamingLLM{name: "primary", err: fmt.Errorf("groq api error (status 429): rate limited")}
	secondary := &mockStreamingLLM{name: "secondary"}
	f := NewChainLLM("test", primary, secondary)

	_, err := f.StreamComplete(ctx, nil, nil,
		func(string) error { return nil },
		func(orchestrator.ToolCallEventData) error { return nil },
	)
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should not be called once the context is already cancelled, got %d calls", secondary.calls)
	}
}
