package orchestrator

import (
	"context"
	"sort"
	"sync"
	"time"
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
// A request cancelled before its provider reported (a reply the caller talked over, a speculation thrown
// away) was still read and partly written by the model, so it is counted as an estimate (AddEstimate,
// providers/llm sentRequest): its prompt at the conversation's own tokens per character, its reply from
// the text that arrived. What the platform races on its own account, a hedge's loser, is not counted.
type SessionMeter struct {
	mu    sync.Mutex
	byCat map[string]*TokenCount
	ratio map[string]float64 // prompt tokens per character of request, by model: the last one reported
	open  int                // requests sent whose counts have not arrived (Begin)
	idle  chan struct{}      // closed when open falls to 0, made by Settle
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
	// Of Prompt and Completion, the tokens estimated for requests no provider reported (AddEstimate).
	EstPrompt     int64 `json:"est_prompt,omitempty"`
	EstCompletion int64 `json:"est_completion,omitempty"`
}

// Token categories. "reply" is the default; the others are set by the call sites that make them.
const (
	TokensReply       = "reply"
	TokensSpeculative = "speculative"
	TokensSummary     = "summary"
	// TokensBackchannel: the short request that picks a listener sound while the caller pauses mid-thought (backchannel.go).
	TokensBackchannel = "backchannel"
	// TokensCandidate: a guess asked from a step a workflow's director may move the call to (turn_director.go
	// TurnCandidates). A host decides whether it bills them.
	TokensCandidate = "candidate"
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

// AddEstimate records a request no provider reported, at our own count of it. The tokens are in Prompt and
// Completion like any other, and in the Est counts so a bill can say which were estimated.
func (m *SessionMeter) AddEstimate(category string, prompt, completion int) {
	if m == nil || (prompt <= 0 && completion <= 0) {
		return
	}
	if category == "" {
		category = TokensReply
	}
	m.Add(category, prompt, completion)
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byCat[category]
	c.EstPrompt += int64(max(prompt, 0))
	c.EstCompletion += int64(max(completion, 0))
}

// LearnRatio remembers how many prompt tokens the model counted for a request of chars characters. A request
// too small to say anything (a hello) is ignored.
func (m *SessionMeter) LearnRatio(model string, tokens, chars int) {
	if m == nil || tokens < 50 || chars < 200 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ratio == nil {
		m.ratio = map[string]float64{}
	}
	m.ratio[model] = float64(tokens) / float64(chars)
}

// Ratio is the tokens per character of the last request of the model that was reported, or 0.
func (m *SessionMeter) Ratio(model string) float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ratio[model]
}

// Begin counts a request as sent until the returned func is called, with its counts recorded.
func (m *SessionMeter) Begin() (done func()) {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	m.open++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.open--
			if m.open == 0 && m.idle != nil {
				close(m.idle)
				m.idle = nil
			}
		})
	}
}

// Settle waits until no request is open, or for wait. A bill drawn as a call ends reads the meter after it:
// the requests the hang-up cancelled are counted a moment later.
func (m *SessionMeter) Settle(wait time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.open == 0 {
		m.mu.Unlock()
		return
	}
	if m.idle == nil {
		m.idle = make(chan struct{})
	}
	idle := m.idle
	m.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-idle:
	case <-t.C:
	}
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
