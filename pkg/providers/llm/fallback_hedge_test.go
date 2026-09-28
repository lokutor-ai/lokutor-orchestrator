package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type fakeStreamer struct {
	name       string
	firstAfter time.Duration
	chunks     []string
	err        error
	calls      atomic.Int32
	cancelled  atomic.Bool
}

func (f *fakeStreamer) Name() string { return f.name }
func (f *fakeStreamer) Complete(ctx context.Context, m []orchestrator.Message, t []orchestrator.Tool) (string, error) {
	return strings.Join(f.chunks, ""), f.err
}
func (f *fakeStreamer) StreamComplete(ctx context.Context, m []orchestrator.Message, t []orchestrator.Tool,
	onChunk func(string) error, onToolCall func(orchestrator.ToolCallEventData) error) (string, error) {
	f.calls.Add(1)
	select {
	case <-time.After(f.firstAfter):
	case <-ctx.Done():
		f.cancelled.Store(true)
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	var out strings.Builder
	for _, c := range f.chunks {
		if err := onChunk(c); err != nil {
			f.cancelled.Store(true)
			return out.String(), err
		}
		out.WriteString(c)
	}
	return out.String(), nil
}

func collect(t *testing.T, c *ChainLLM) (string, string, time.Duration, error) {
	t.Helper()
	var mu sync.Mutex
	var got strings.Builder
	start := time.Now()
	text, err := c.StreamComplete(context.Background(), nil, nil, func(s string) error {
		mu.Lock()
		got.WriteString(s)
		mu.Unlock()
		return nil
	}, func(orchestrator.ToolCallEventData) error { return nil })
	return text, got.String(), time.Since(start), err
}

func TestChainHedge_FastPrimaryNeverHedges(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "200")
	a := &fakeStreamer{name: "a", firstAfter: 20 * time.Millisecond, chunks: []string{"Hola", " mundo"}}
	b := &fakeStreamer{name: "b", firstAfter: 10 * time.Millisecond, chunks: []string{"B"}}
	text, streamed, _, err := collect(t, NewChainLLM("t", a, b))
	if err != nil || text != "Hola mundo" || streamed != "Hola mundo" || b.calls.Load() != 0 {
		t.Fatalf("text=%q streamed=%q err=%v b.calls=%d", text, streamed, err, b.calls.Load())
	}
}

func TestChainHedge_SlowPrimaryIsBeatenBySecondary(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "200")
	a := &fakeStreamer{name: "a", firstAfter: 3 * time.Second, chunks: []string{"late"}}
	b := &fakeStreamer{name: "b", firstAfter: 100 * time.Millisecond, chunks: []string{"Rápido", "."}}
	text, streamed, took, err := collect(t, NewChainLLM("t", a, b))
	if err != nil || text != "Rápido." || streamed != "Rápido." {
		t.Fatalf("text=%q streamed=%q err=%v", text, streamed, err)
	}
	if took > time.Second {
		t.Fatalf("took %v; the hedge should answer at ~300ms", took)
	}
	time.Sleep(50 * time.Millisecond)
	if !a.cancelled.Load() {
		t.Fatalf("the losing attempt must be cancelled")
	}
}

func TestChainHedge_PrimaryErrorFailsOver(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "500")
	a := &fakeStreamer{name: "a", firstAfter: 0, err: errors.New("429 rate limited")}
	b := &fakeStreamer{name: "b", firstAfter: 10 * time.Millisecond, chunks: []string{"ok"}}
	text, _, took, err := collect(t, NewChainLLM("t", a, b))
	if err != nil || text != "ok" || took > 400*time.Millisecond {
		t.Fatalf("failover must not wait for the hedge timer: text=%q err=%v took=%v", text, err, took)
	}
}

func TestChainHedge_BothFail(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "50")
	a := &fakeStreamer{name: "a", firstAfter: 100 * time.Millisecond, err: errors.New("boom a")}
	b := &fakeStreamer{name: "b", firstAfter: 10 * time.Millisecond, err: errors.New("boom b")}
	if _, _, _, err := collect(t, NewChainLLM("t", a, b)); err == nil {
		t.Fatalf("expected an error")
	}
}

func TestChainHedge_DisabledIsSequential(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "0")
	a := &fakeStreamer{name: "a", firstAfter: 300 * time.Millisecond, chunks: []string{"a"}}
	b := &fakeStreamer{name: "b", firstAfter: 0, chunks: []string{"b"}}
	text, _, _, err := collect(t, NewChainLLM("t", a, b))
	if err != nil || text != "a" || b.calls.Load() != 0 {
		t.Fatalf("text=%q err=%v b.calls=%d", text, err, b.calls.Load())
	}
}

// When every provider fails, each one's error is reported by name, not only the last. On 2026-09-29
// the backup's instant 429 (Groq's 8,000 tokens/minute) was the only error logged for five silent
// turns, and the primary's failure -- the one that mattered -- was lost.
func TestHedgedChainReportsEveryProvidersFailure(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "50")
	primaryErr := errors.New("cerebras api error (status 400): bad tool message")
	backupErr := errors.New("groq api error (status 429): rate limit")
	primary := &fakeStreamer{name: "cerebras", firstAfter: 10 * time.Millisecond, err: primaryErr}
	backup := &fakeStreamer{name: "groq", err: backupErr}
	c := NewChainLLM("test", primary, backup)

	_, err := c.StreamComplete(context.Background(), nil, nil,
		func(string) error { return nil }, func(orchestrator.ToolCallEventData) error { return nil })
	if err == nil {
		t.Fatal("expected an error when every provider fails")
	}
	if !errors.Is(err, primaryErr) || !errors.Is(err, backupErr) {
		t.Fatalf("both providers' errors must be reachable, got %v", err)
	}
	for _, want := range []string{"cerebras:", "bad tool message", "groq:", "429"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}
