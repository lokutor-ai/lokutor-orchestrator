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
