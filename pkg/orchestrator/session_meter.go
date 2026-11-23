package orchestrator

import (
	"context"
	"sort"
	"sync"
)

// SessionMeter adds up every language-model token a provider reports on behalf of one conversation,
// by category, so the conversation can be billed for exactly what reached the model.
//
// It is installed once, on the session's context (ManagedStream does it), and providers write to it
// from the same place they already report usage (providers/llm recordUsage). Every request that
// derives its context from the session's is counted without its call site knowing about billing: the
// turn's reply and its tool rounds, speculative replies, the silence check-in, the history fold. A
// call site that wants its tokens shown under their own heading sets a category on its context
// (WithTokenCategory); the rest count as "reply".
//
// A request whose provider never reported usage (a stream cancelled before its usage chunk, such as
// a hedge's loser or a superseded speculation) is not counted: it is absorbed, not estimated.
type SessionMeter struct {
	mu    sync.Mutex
	byCat map[string]*TokenCount
}

// TokenCount is the tokens of one category.
type TokenCount struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Requests   int   `json:"requests"`
	// Of Prompt, the tokens the provider's prompt cache served and wrote, and the provider's own price
	// for the requests (USD), where it reports them. For our cost, not the customer's bill: the bill
	// prices Prompt whole.
	CachedPrompt int64   `json:"cached_prompt,omitempty"`
	CacheWrite   int64   `json:"cache_write,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Token categories. "reply" is the default; the others are set by the call sites that make them.
const (
	TokensReply       = "reply"
	TokensSpeculative = "speculative"
	TokensSummary     = "summary"
	// TokensBackchannel: the short request that picks a listener sound while the caller pauses mid-thought (backchannel.go).
	TokensBackchannel = "backchannel"
)

// NewSessionMeter returns an empty meter.
func NewSessionMeter() *SessionMeter { return &SessionMeter{byCat: map[string]*TokenCount{}} }

// Add records one provider report. Safe on a nil meter, so callers need no check.
func (m *SessionMeter) Add(category string, prompt, completion int) {
	if m == nil || (prompt <= 0 && completion <= 0) {
		return
	}
	if category == "" {
		category = TokensReply
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byCat[category]
	if c == nil {
		c = &TokenCount{}
		m.byCat[category] = c
	}
	c.Prompt += int64(max(prompt, 0))
	c.Completion += int64(max(completion, 0))
	c.Requests++
}

// AddCache records the cache side and cost of one provider report, under the category Add used. It
// does not count a request: Add does.
func (m *SessionMeter) AddCache(category string, c TokenCache) {
	if m == nil || (c.CachedTokens <= 0 && c.CacheWriteTokens <= 0 && c.CostUSD <= 0) {
		return
	}
	if category == "" {
		category = TokensReply
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.byCat[category]
	if t == nil {
		t = &TokenCount{}
		m.byCat[category] = t
	}
	t.CachedPrompt += int64(max(c.CachedTokens, 0))
	t.CacheWrite += int64(max(c.CacheWriteTokens, 0))
	t.CostUSD += max(c.CostUSD, 0)
}

// Snapshot returns a copy of the counts so far, by category.
func (m *SessionMeter) Snapshot() map[string]TokenCount {
	out := map[string]TokenCount{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.byCat {
		out[k] = *v
	}
	return out
}

// Categories returns the categories recorded so far, sorted, for stable logs.
func (m *SessionMeter) Categories() []string {
	snap := m.Snapshot()
	out := make([]string, 0, len(snap))
	for k := range snap {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type sessionMeterKey struct{}
type tokenCategoryKey struct{}

// WithSessionMeter attaches m to ctx.
func WithSessionMeter(ctx context.Context, m *SessionMeter) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionMeterKey{}, m)
}

// SessionMeterFrom returns the meter on ctx, or nil.
func SessionMeterFrom(ctx context.Context) *SessionMeter {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(sessionMeterKey{}).(*SessionMeter)
	return m
}

// WithTokenCategory files the tokens of requests made with ctx under category.
func WithTokenCategory(ctx context.Context, category string) context.Context {
	return context.WithValue(ctx, tokenCategoryKey{}, category)
}

// TokenCategoryFrom is the category set on ctx, or "reply".
func TokenCategoryFrom(ctx context.Context) string {
	if ctx != nil {
		if c, ok := ctx.Value(tokenCategoryKey{}).(string); ok && c != "" {
			return c
		}
	}
	return TokensReply
}
