package orchestrator

import (
	"context"
	"strings"
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
	// answeredBy is the provider a chain's turn came from. Since 2026-09-29 the second provider is a
	// different model at a different price, so which one answered is part of what a turn cost and said.
	answeredBy string
	// runOns are the replies a provider cut short because the model went on past its turn (see
	// providers/llm oneTurn): what it said and did not get to say is part of what the turn said.
	runOns []*RunOnCut
}

// RunOnCut is one reply cut where the model's next message began. Dropped grows after the cut, as
// the provider reads the rest of the stream in the background.
type RunOnCut struct {
	mu      sync.Mutex
	reason  string
	kept    string
	dropped strings.Builder
}

// NoteRunOn records a cut and returns it for the provider to add the dropped text to. It never
// returns nil, so the provider need not check for a sink.
func (u *TokenUsage) NoteRunOn(reason, kept string) *RunOnCut {
	c := &RunOnCut{reason: reason, kept: kept}
	if u == nil {
		return c
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.runOns = append(u.runOns, c)
	return c
}

// TakeRunOns returns the cuts recorded since the last call.
func (u *TokenUsage) TakeRunOns() []*RunOnCut {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.runOns
	u.runOns = nil
	return out
}

// Drop adds text the reply did not keep.
func (c *RunOnCut) Drop(s string) {
	if c == nil || s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped.WriteString(s)
}

// Reason, Kept and Dropped describe the cut for the log.
func (c *RunOnCut) Reason() string { return c.reason }
func (c *RunOnCut) Kept() string   { return c.kept }
func (c *RunOnCut) Dropped() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped.String()
}

// SetAnsweredBy records the provider that produced the output. A tool round is a second call, and the
// last one is what the caller hears, so a later call overwrites.
func (u *TokenUsage) SetAnsweredBy(name string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answeredBy = name
}

// AnsweredBy is the provider recorded by SetAnsweredBy, or "" when nothing recorded one (a single
// provider rather than a chain).
func (u *TokenUsage) AnsweredBy() string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.answeredBy
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
