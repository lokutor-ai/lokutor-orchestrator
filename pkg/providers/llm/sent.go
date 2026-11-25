package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"unicode/utf8"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// sentRequest is one request to one of the platform's providers, and what it is counted as when its usage
// never arrives. A streaming reply's counts come in its last chunk, so a reply cancelled on the way (the
// caller talked over it, a speculation was thrown away, the call ended) reports none, and the model has
// been paid for all the same. It is counted as an estimate in the session's meter:
//
//   - prompt: the request's characters at the tokens per character of the last request of the same model
//     this session had reported (the last count, plus the growth since), or the model's usual ratio before
//     there is one;
//   - reply: the text, thinking and tool arguments that had arrived.
//
// Not counted: a request the provider refused or broke off, one that never left, and what the platform
// races on its own account (ChainLLM's hedge, see withCancelBilled).
type sentRequest struct {
	ctx      context.Context
	model    string
	chars    int // characters of the request
	sent     atomic.Bool
	answered bool // 200
	reported bool
	failed   bool // refused, or broken off by the provider
	stopped  bool // the reader stopped it (a callback's error), with the context alive
	reply    int  // characters of the reply
	done     func()
}

// startRequest counts a request of body, the JSON sent. end must be called.
func startRequest(ctx context.Context, model string, body []byte) *sentRequest {
	return &sentRequest{ctx: ctx, model: model, chars: utf8.RuneCount(body), done: orchestrator.SessionMeterFrom(ctx).Begin()}
}

// trace is req watched for the moment it has been written whole.
func (s *sentRequest) trace(req *http.Request) *http.Request {
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				s.sent.Store(true)
			}
		},
	}))
}

// chunk reads the usage in one streaming chunk, and counts the reply in it. Call it for every chunk, before any
// choices-based early return.
func (s *sentRequest) chunk(data []byte) {
	if s.reported {
		return
	}
	var c struct {
		Usage   *usagePayload `json:"usage"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	for _, ch := range c.Choices {
		s.reply += utf8.RuneCountInString(ch.Delta.Content) + utf8.RuneCountInString(ch.Delta.Reasoning)
		for _, tc := range ch.Delta.ToolCalls {
			s.reply += utf8.RuneCountInString(tc.Function.Name) + utf8.RuneCountInString(tc.Function.Arguments)
		}
	}
	if c.Usage != nil {
		s.usage(*c.Usage)
	}
}

// usage records what the provider reported.
func (s *sentRequest) usage(u usagePayload) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return
	}
	s.reported = true
	recordUsage(s.ctx, u)
	orchestrator.SessionMeterFrom(s.ctx).LearnRatio(s.model, u.PromptTokens, s.chars)
}

// end closes the request, counting it as an estimate if it has to be.
func (s *sentRequest) end() {
	defer s.done()
	m := orchestrator.SessionMeterFrom(s.ctx)
	if m == nil || s.reported || s.failed || !s.counts() {
		return
	}
	if !cancelBilled(s.ctx) {
		return
	}
	m.AddEstimate(orchestrator.TokenCategoryFrom(s.ctx), s.promptEstimate(m), int(float64(s.reply)*usualRatio(s.model)+0.5))
}

// counts reports whether the model worked on the request though it reported nothing.
func (s *sentRequest) counts() bool {
	switch err := s.ctx.Err(); {
	case errors.Is(err, context.Canceled):
		// The caller went on, or left. Once the request has gone out the model is reading it, whether or not its
		// answer had begun.
		return s.sent.Load()
	case err != nil:
		// Our own time limit: only if the provider had taken the request. One that never answered is its failure.
		return s.answered
	case s.stopped:
		return s.answered
	default:
		// It ended by itself without a report (a host that sends none).
		return s.answered && s.reply > 0
	}
}

func (s *sentRequest) promptEstimate(m *orchestrator.SessionMeter) int {
	r := m.Ratio(s.model)
	if r <= 0 {
		r = usualRatio(s.model)
	}
	return int(float64(s.chars)*r + 0.5)
}

// usualRatio is tokens per character before a session has a count of its own: four characters to a token, and
// Claude's tokenizer makes 1.54 times as many (finance/measurements/talker_2026-11-11).
func usualRatio(model string) float64 {
	if isAnthropicModel(model) {
		return 0.385
	}
	return 0.25
}

type cancelBilledKey struct{}

// withCancelBilled attaches the answer to "is this attempt's cancelled request the customer's?": a chain races
// two providers for the platform's sake, and only one of them is the reply asked for.
func withCancelBilled(ctx context.Context, f func() bool) context.Context {
	if outer, ok := ctx.Value(cancelBilledKey{}).(func() bool); ok {
		inner := f
		f = func() bool { return inner() && outer() }
	}
	return context.WithValue(ctx, cancelBilledKey{}, f)
}

func cancelBilled(ctx context.Context) bool {
	f, ok := ctx.Value(cancelBilledKey{}).(func() bool)
	return !ok || f()
}

// broke marks a reply whose stream ended with an error of the provider's or the network's, not of ours.
func (s *sentRequest) broke() {
	if s.ctx.Err() == nil {
		s.failed = true
	}
}
