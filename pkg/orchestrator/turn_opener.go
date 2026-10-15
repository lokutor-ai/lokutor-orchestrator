package orchestrator

import "context"

type turnOpenerKey struct{}

// WithTurnOpener marks a synthesis context as the turn's first sentence: the one the caller is waiting on, since the
// later sentences of a reply are synthesized while it plays. A TTS provider that batches requests can let it skip the
// wait for a batch to form: on 2026-10-16 the synthesis server held every request up to 150 ms under load, including
// this one (lokutor_tts cmd/versa_server).
func WithTurnOpener(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnOpenerKey{}, true)
}

// TurnOpener reports whether ctx is the synthesis of a turn's first sentence (WithTurnOpener).
func TurnOpener(ctx context.Context) bool {
	v, _ := ctx.Value(turnOpenerKey{}).(bool)
	return v
}
