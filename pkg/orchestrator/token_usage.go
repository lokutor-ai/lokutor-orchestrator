package orchestrator

import (
	"context"
	"sync"
)

// Token accounting for language-model calls.
//
// This exists to remove the single estimated number in the unit-economics report. Language-model
// cost is 42% of variable cost per call-minute and it was the only figure in that document derived
// from an assumption — "roughly five turns a minute, about 600 input and 40 output tokens each" —
// rather than from measurement. An estimate carrying the largest share of variable cost is the one
// most worth replacing, and it is also the one that silently goes stale: prompts grow, history
// accumulates, and nobody re-derives it.
//
// The shape is deliberately a context-carried sink rather than a change to LLMProvider. Adding a
// return value to StreamComplete would touch six provider implementations and every call site for
// a number most callers do not want, and storing the last usage ON the provider would attribute
// tokens to whichever session read it first — providers are shared across concurrent calls. A sink
// created per call and passed down the context is private to that call, costs nothing when absent,
// and lets a provider that cannot report usage simply not write to it.

// TokenUsage collects the token counts for one language-model call. The zero value is ready to use;
// a nil *TokenUsage is safe to Record into, which is what makes the "no sink installed" path free.
type TokenUsage struct {
	mu               sync.Mutex
	promptTokens     int
	completionTokens int
	totalTokens      int
	reported         bool
}

// Record stores the counts a provider parsed. Later calls accumulate rather than overwrite: one
// logical turn can make several provider calls (a tool round-trip is two), and the turn's cost is
// their sum, not the last one.
func (u *TokenUsage) Record(prompt, completion, total int) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.promptTokens += prompt
	u.completionTokens += completion
	if total > 0 {
		u.totalTokens += total
	} else {
		u.totalTokens += prompt + completion
	}
	u.reported = true
}

// Snapshot returns the accumulated counts. ok is false when no provider reported anything, which is
// meaningfully different from a genuine zero and must not be logged as "0 tokens".
func (u *TokenUsage) Snapshot() (prompt, completion, total int, ok bool) {
	if u == nil {
		return 0, 0, 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.promptTokens, u.completionTokens, u.totalTokens, u.reported
}

type tokenUsageKey struct{}

// WithTokenUsage attaches a sink to ctx. Callers that want token counts create a TokenUsage, pass
// the derived context to the provider, and read Snapshot afterwards.
func WithTokenUsage(ctx context.Context, u *TokenUsage) context.Context {
	if u == nil {
		return ctx
	}
	return context.WithValue(ctx, tokenUsageKey{}, u)
}

// TokenUsageFrom returns the sink on ctx, or nil. Providers call this and Record unconditionally —
// Record tolerates nil, so there is no branch to forget.
func TokenUsageFrom(ctx context.Context) *TokenUsage {
	if ctx == nil {
		return nil
	}
	u, _ := ctx.Value(tokenUsageKey{}).(*TokenUsage)
	return u
}
