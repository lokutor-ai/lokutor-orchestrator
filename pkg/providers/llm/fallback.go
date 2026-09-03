package llm

import (
	"context"
	"fmt"
	"strings"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// isRateLimited reports whether err looks like a rate-limit/quota rejection
// rather than a genuine failure (bad request, model error, network issue).
// Every LLM provider in this package embeds the HTTP status code in its
// error text as "... (status %d): ..." — checking for 429 there covers all
// of them; RESOURCE_EXHAUSTED is Gemini's specific quota-exceeded reason
// string, kept as a defensive backup in case the status code isn't
// surfaced for some response shape.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 429") ||
		strings.Contains(msg, "RESOURCE_EXHAUSTED") ||
		strings.Contains(msg, "rate_limit")
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
		if err == nil || ctx.Err() != nil || !isRateLimited(err) {
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

		if err == nil || started || ctx.Err() != nil || !isRateLimited(err) {
			return text, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no LLM providers configured in chain %q", c.name)
	}
	return "", lastErr
}
