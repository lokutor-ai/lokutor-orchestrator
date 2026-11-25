package orchestrator

import "context"

type reasoningEffortKey struct{}

// WithReasoningEffort asks the providers that send a reasoning effort (the gpt-oss models) to use
// effort ("low", "medium", "high") for the requests made with the returned context, instead of the
// one they are configured with. Providers that send none ignore it. It exists for asking again: the
// configured "low" is what keeps the first spoken word fast, and "medium" writes a tool call out as
// text far less often (llm.forGptOss), at roughly twice the time to the first token, which a second
// try can afford and every first try cannot.
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if effort == "" {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortKey{}, effort)
}

// ReasoningEffortFrom is the effort set on ctx by WithReasoningEffort, or "".
func ReasoningEffortFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(reasoningEffortKey{}).(string)
	return s
}

type maxTokensKey struct{}

// WithMaxTokens caps the completion of the requests made with the returned context at n tokens, on the providers
// that answer without reasoning first (OpenRouter with reasoning off; a reasoning model would spend the cap thinking
// and return nothing, so the gpt-oss providers ignore it). For requests whose whole answer is a word or two.
func WithMaxTokens(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, maxTokensKey{}, n)
}

// MaxTokensFrom is the cap set on ctx by WithMaxTokens, or 0.
func MaxTokensFrom(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	n, _ := ctx.Value(maxTokensKey{}).(int)
	return n
}

type backgroundSynthesisKey struct{}

// WithBackgroundSynthesis marks synthesis requests made with the returned context as the stream's own background work
// (its backchannel sounds), not the reply: a TTS provider must not let a barge-in's Abort() reach for them instead of
// the reply it should stop, nor count them as the reply in flight.
func WithBackgroundSynthesis(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundSynthesisKey{}, true)
}

// IsBackgroundSynthesis reports whether ctx was made by WithBackgroundSynthesis.
func IsBackgroundSynthesis(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	b, _ := ctx.Value(backgroundSynthesisKey{}).(bool)
	return b
}

type temperatureKey struct{}

// WithTemperature sets the sampling temperature of the requests made with the returned context, on the providers that take
// one (OpenRouter). Without it a provider's own default applies (1 for Claude).
func WithTemperature(ctx context.Context, t float64) context.Context {
	return context.WithValue(ctx, temperatureKey{}, t)
}

// TemperatureFrom is the temperature set on ctx, if one is.
func TemperatureFrom(ctx context.Context) (float64, bool) {
	t, ok := ctx.Value(temperatureKey{}).(float64)
	return t, ok
}

type sentOnceKey struct{}

// WithSentOnce marks requests made with the returned context as prompts no later request repeats: an Anthropic
// model's prompt is then not written to its cache (the write costs 1.25 times the input and is earned back only by
// a second request that reads it).
func WithSentOnce(ctx context.Context) context.Context {
	return context.WithValue(ctx, sentOnceKey{}, true)
}

// SentOnce reports whether ctx was marked by WithSentOnce.
func SentOnce(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	on, _ := ctx.Value(sentOnceKey{}).(bool)
	return on
}
