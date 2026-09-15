package llm

import (
	"context"
	"fmt"

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

func (c *ChainLLM) StreamComplete(
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
