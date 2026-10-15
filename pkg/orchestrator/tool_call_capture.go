package orchestrator

import (
	"context"
	"sync"
)

// ToolCallCapture collects the tool calls a non-streaming Complete received, for the caller that installed it with
// WithToolCallCapture. Complete returns only the reply's text, so without it a reply that called a tool read as words
// alone: the speculative run (speculator.go) used to get "" for a call, or the words said before it, and the call was
// lost. Providers add what they parse; a provider that does not leaves it empty, which is what Complete always meant.
type ToolCallCapture struct {
	mu    sync.Mutex
	calls []ToolCallEventData
}

type toolCallCaptureKey struct{}

// WithToolCallCapture returns ctx carrying a fresh capture, and the capture.
func WithToolCallCapture(ctx context.Context) (context.Context, *ToolCallCapture) {
	c := &ToolCallCapture{}
	return context.WithValue(ctx, toolCallCaptureKey{}, c), c
}

// ToolCallCaptureFrom returns the capture on ctx, or nil. Add tolerates nil.
func ToolCallCaptureFrom(ctx context.Context) *ToolCallCapture {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(toolCallCaptureKey{}).(*ToolCallCapture)
	return c
}

// Add records one call.
func (c *ToolCallCapture) Add(tc ToolCallEventData) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.calls = append(c.calls, tc)
	c.mu.Unlock()
}

// Calls returns what was recorded, in order.
func (c *ToolCallCapture) Calls() []ToolCallEventData {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ToolCallEventData(nil), c.calls...)
}

// replayedRound answers the first StreamComplete of a turn with a round already received for it (the speculative
// run's words and tool calls, for the same transcript), and passes every later request to the real provider: the
// rounds after the tools, and a retry. The turn then runs exactly as if that first request had been made now; the
// tools still run only once the turn is confirmed.
type replayedRound struct {
	real  StreamingLLMProvider
	text  string
	calls []ToolCallEventData
	// tokens are what the speculative run spent on this round. They go into the turn's sink so the turn-latency line
	// carries them; the conversation's bill (SessionMeter) has them already, from the run itself.
	tokens *TokenUsage
	mu     sync.Mutex
	used   bool
}

// specToolRound is what trySpeculativeResponse hands runLLMAndTTS: a tool round for transcript.
type specToolRound struct {
	transcript string
	text       string
	calls      []ToolCallEventData
	tokens     *TokenUsage
}

func (r *replayedRound) Name() string { return r.real.Name() }

func (r *replayedRound) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return r.real.Complete(ctx, messages, tools)
}

func (r *replayedRound) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	r.mu.Lock()
	first := !r.used
	r.used = true
	r.mu.Unlock()
	if !first {
		return r.real.StreamComplete(ctx, messages, tools, onChunk, onToolCall)
	}
	if p, c, t, ok := r.tokens.Snapshot(); ok {
		sink := TokenUsageFrom(ctx)
		sink.Record(p, c, t)
		sink.SetAnsweredBy(r.tokens.AnsweredBy())
	}
	if r.text != "" && onChunk != nil {
		if err := onChunk(r.text); err != nil {
			return "", err
		}
	}
	for _, tc := range r.calls {
		if onToolCall != nil {
			if err := onToolCall(tc); err != nil {
				return "", err
			}
		}
	}
	return r.text, nil
}
