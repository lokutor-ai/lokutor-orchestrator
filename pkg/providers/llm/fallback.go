package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// shouldFailover decides whether the next provider in the chain gets a turn.
//
// This used to match rate-limit-shaped errors only (HTTP 429, Gemini's
// RESOURCE_EXHAUSTED), which quietly made the first provider a single point of
// failure for everything except quota: an expired or revoked
// key answers 401, a wrong model name answers 404, a rejected parameter answers
// 400, and a provider outage answers 5xx or fails to connect before any status
// exists at all. Every one of those returned the error straight to the caller
// with the healthy providers behind it never tried — a broken agent while a
// working backend sits idle one slot down the chain, which is exactly the
// failure the chain exists to prevent.
//
// We only reach here when the provider produced no output whatsoever, so trying
// the next one cannot duplicate or truncate speech. That makes "it failed and
// said nothing" sufficient grounds on its own, and no error class is worth
// spending a whole turn's silence on rather than one more round trip. Context
// cancellation is the exception and is checked by the caller: that turn is
// already gone, and a barge-in must not fan out across the whole chain.
func shouldFailover(err error) bool {
	return err != nil
}

// ChainLLM wraps an ordered list of LLM providers and automatically retries
// on the next one when the current provider fails with a rate-limit-shaped
// error. This is how several separate free/low tiers (or a fast-but-tight
// paid tier plus slower backups) add up to combined throughput instead of
// the voice agent being capped by whichever provider is first — e.g. a
// Cerebras + Groq + Gemini chain adds each provider's own quota together,
// and any single provider having an outage doesn't take the voice agent
// down with it.
//
// Failover only happens before any output has reached the caller (no chunk
// emitted, no tool call dispatched) for the provider currently being tried —
// a rate limit hit mid-stream is reported as an error rather than retried,
// so a failover can never cause duplicate or truncated speech from two
// providers both having produced partial output for the same turn.
type ChainLLM struct {
	providers []orchestrator.LLMProvider
	name      string
}

// NewChainLLM builds a ChainLLM tried in the given order. name is used for
// logging/diagnostics (e.g. "cerebras+groq+gemini chain").
func NewChainLLM(name string, providers ...orchestrator.LLMProvider) *ChainLLM {
	return &ChainLLM{providers: providers, name: name}
}

func (c *ChainLLM) Name() string { return c.name }

func (c *ChainLLM) Complete(ctx context.Context, messages []orchestrator.Message, tools []orchestrator.Tool) (string, error) {
	var lastErr error
	for _, p := range c.providers {
		text, err := p.Complete(ctx, messages, tools)
		if err == nil || ctx.Err() != nil || !shouldFailover(err) {
			return text, err
		}
		lastErr = err
	}
	return "", lastErr
}

// llmHedgeDelay is how long the first provider has to produce its first token before the next one
// is started alongside it. A voice turn waits on the first token, and it is the tail that hurts:
// on 2026-09-23 the model's first token was 265 ms at the median and 703 ms at p90, but 1.6 s,
// 1.85 s and twice more than 3 s on individual turns — the last two long enough to trigger the
// "let me think" filler. Racing the second provider past p90 turns those into ~p90 turns, and
// costs a second request on only the slowest tenth of turns. LLM_HEDGE_MS overrides; 0 disables.
func llmHedgeDelay() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LLM_HEDGE_MS"))); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 800 * time.Millisecond
}

// errHedgeLost stops the stream of an attempt that another attempt beat to its first output.
var errHedgeLost = errors.New("llm hedge: another provider answered first")

func (c *ChainLLM) StreamComplete(
	ctx context.Context,
	messages []orchestrator.Message,
	tools []orchestrator.Tool,
	onChunk func(string) error,
	onToolCall func(orchestrator.ToolCallEventData) error,
) (string, error) {
	if hedge := llmHedgeDelay(); hedge > 0 && len(c.providers) >= 2 {
		if _, ok := c.providers[1].(orchestrator.StreamingLLMProvider); ok {
			return c.streamHedged(ctx, messages, tools, onChunk, onToolCall, hedge)
		}
	}
	return c.streamSequential(ctx, messages, tools, onChunk, onToolCall)
}

func (c *ChainLLM) streamSequential(
	ctx context.Context,
	messages []orchestrator.Message,
	tools []orchestrator.Tool,
	onChunk func(string) error,
	onToolCall func(orchestrator.ToolCallEventData) error,
) (string, error) {
	var lastErr error
	for _, p := range c.providers {
		started := false
		trackChunk := func(s string) error {
			started = true
			return onChunk(s)
		}
		trackToolCall := func(tc orchestrator.ToolCallEventData) error {
			started = true
			return onToolCall(tc)
		}

		var text string
		var err error
		if streamer, ok := p.(orchestrator.StreamingLLMProvider); ok {
			text, err = streamer.StreamComplete(ctx, messages, tools, trackChunk, trackToolCall)
		} else {
			text, err = p.Complete(ctx, messages, tools)
		}

		if err == nil || started || ctx.Err() != nil || !shouldFailover(err) {
			return text, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no LLM providers configured in chain %q", c.name)
	}
	return "", lastErr
}

// streamHedged runs providers[0], starts providers[1] alongside it if no output has arrived after
// hedge, and streams whichever produces output first; the other is cancelled. An attempt that fails
// before producing anything falls through to the next provider, as the sequential chain does.
func (c *ChainLLM) streamHedged(
	ctx context.Context,
	messages []orchestrator.Message,
	tools []orchestrator.Tool,
	onChunk func(string) error,
	onToolCall func(orchestrator.ToolCallEventData) error,
	hedge time.Duration,
) (string, error) {
	type result struct {
		idx  int
		text string
		err  error
	}
	var (
		mu      sync.Mutex
		winner  = -1
		cancels = make([]context.CancelFunc, len(c.providers))
	)
	results := make(chan result, len(c.providers))
	running := 0

	// claim makes attempt i the winner if nobody has produced output yet, cancelling the rest.
	claim := func(i int) bool {
		mu.Lock()
		defer mu.Unlock()
		if winner == -1 {
			winner = i
			for j, cancel := range cancels {
				if j != i && cancel != nil {
					cancel()
				}
			}
		}
		return winner == i
	}
	start := func(i int) {
		actx, cancel := context.WithCancel(ctx)
		mu.Lock()
		cancels[i] = cancel
		mu.Unlock()
		running++
		p := c.providers[i]
		go func() {
			chunk := func(s string) error {
				if !claim(i) {
					return errHedgeLost
				}
				return onChunk(s)
			}
			toolCall := func(tc orchestrator.ToolCallEventData) error {
				if !claim(i) {
					return errHedgeLost
				}
				return onToolCall(tc)
			}
			var text string
			var err error
			if streamer, ok := p.(orchestrator.StreamingLLMProvider); ok {
				text, err = streamer.StreamComplete(actx, messages, tools, chunk, toolCall)
			} else {
				text, err = p.Complete(actx, messages, tools)
			}
			results <- result{i, text, err}
		}()
	}
	defer func() {
		mu.Lock()
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
		mu.Unlock()
	}()

	start(0)
	next := 1
	timer := time.NewTimer(hedge)
	defer timer.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			mu.Lock()
			none := winner == -1
			mu.Unlock()
			if none && next < 2 {
				start(next)
				next++
			}
		case r := <-results:
			running--
			mu.Lock()
			w := winner
			mu.Unlock()
			switch {
			case w == r.idx:
				// The winner finished, cleanly or not: its outcome is the turn's.
				return r.text, r.err
			case w != -1:
				// A loser wound down after being cancelled; keep waiting for the winner.
				continue
			case r.err == nil:
				// Finished without streaming anything (an empty reply, or a non-streaming
				// provider): that is still the answer.
				if claim(r.idx) {
					return r.text, nil
				}
				continue
			}
			// Failed before producing anything.
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = r.err
			if running > 0 {
				continue // the other attempt may still answer
			}
			if next < len(c.providers) && shouldFailover(r.err) {
				start(next)
				next++
				continue
			}
			return "", lastErr
		}
	}
}
