package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type replyIDKey struct{}

// WithReply marks a synthesis context as one sentence of reply `id`: every sentence of one agent turn carries the same id.
// A TTS provider whose model can carry a reply on in one running stream (the Versa AR pool's reply sessions,
// lokutor_tts cmd/versa_server) appends each sentence to the previous one's stream instead of starting a fresh one, so
// the delivery carries on from the speech itself.
func WithReply(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, replyIDKey{}, id)
}

// ReplyID is the reply a synthesis context belongs to (WithReply), or "".
func ReplyID(ctx context.Context) string {
	v, _ := ctx.Value(replyIDKey{}).(string)
	return v
}

// ReplyEnder is an optional TTSProvider capability: told that a reply has no more sentences, it releases whatever it
// held for the next one (a synthesis slot) at once rather than at a timeout.
type ReplyEnder interface {
	EndReply(id string)
}

// newReplyID is a random id for one turn's reply: unique across every worker pod sharing a synthesis pool.
func newReplyID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
