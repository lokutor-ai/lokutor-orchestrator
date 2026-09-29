package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Replays session agent_1790600579818180595 (2026-09-28, landing demo, Spanish) through the real
// pipeline: a caller asks for a booking, starts speaking again before the reply plays (its two
// sentences are discarded), finishes the thought and is answered, then goes quiet.
//
// In production the caller then heard the booking question answered AGAIN, 18 s later, behind a
// "turn latency" line reading ck_pre_llm_ms 18716 with every other checkpoint at zero — which read
// as a turn held in a gate and answered stale. No turn was held. It was the silence nudge: the model
// was re-asked with nothing new in the conversation and re-answered its last question, and the line
// measured the nudge from the previous turn's sttEnd. Meanwhile the discarded reply, which the caller
// never heard a word of, sat in context and in the transcript as the agent's answer.

const discardLine = "Discarding generated response: caller started speaking again before playback began"

// replayLog keeps every line so the test can assert on what was — and was not — logged.
type replayLog struct {
	mu    sync.Mutex
	lines []replayLine
}

type replayLine struct {
	msg string
	kv  map[string]interface{}
}

func (r *replayLog) add(msg string, args ...interface{}) {
	kv := map[string]interface{}{}
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			kv[k] = args[i+1]
		}
	}
	r.mu.Lock()
	r.lines = append(r.lines, replayLine{msg, kv})
	r.mu.Unlock()
}

func (r *replayLog) Debug(msg string, args ...interface{}) { r.add(msg, args...) }
func (r *replayLog) Info(msg string, args ...interface{})  { r.add(msg, args...) }
func (r *replayLog) Warn(msg string, args ...interface{})  { r.add(msg, args...) }
func (r *replayLog) Error(msg string, args ...interface{}) { r.add(msg, args...) }

func (r *replayLog) find(msg string) []replayLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []replayLine
	for _, l := range r.lines {
		if l.msg == msg {
			out = append(out, l)
		}
	}
	return out
}

// replaySTT hands out one transcript per utterance, in order.
type replaySTT struct {
	mu    sync.Mutex
	texts []string
	n     int
}

func (s *replaySTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.texts[len(s.texts)-1]
	if s.n < len(s.texts) {
		t = s.texts[s.n]
	}
	s.n++
	return TranscriptionResult{Text: t}, nil
}

func (s *replaySTT) Name() string { return "ReplaySTT" }

// replayLLM streams one scripted reply per call and keeps every request as the model received it.
// beforeReply runs once the request is recorded and before any text streams: the moment a caller
// resuming mid-generation is simulated.
type replayLLM struct {
	mu          sync.Mutex
	replies     []string
	requests    [][]Message
	beforeReply func(call int)
}

func (l *replayLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return l.StreamComplete(ctx, messages, tools, nil, nil)
}

func (l *replayLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	l.mu.Lock()
	call := len(l.requests)
	l.requests = append(l.requests, append([]Message(nil), messages...))
	reply := l.replies[len(l.replies)-1]
	if call < len(l.replies) {
		reply = l.replies[call]
	}
	hook := l.beforeReply
	l.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if onChunk != nil {
		_ = onChunk(reply)
	}
	return reply, nil
}

func (l *replayLLM) Name() string { return "ReplayLLM" }

func (l *replayLLM) calls() [][]Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]Message(nil), l.requests...)
}

// lastSpoken is the last message of a request that is not a system message.
func lastSpoken(msgs []Message) (Message, int) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "system" {
			return msgs[i], i
		}
	}
	return Message{}, -1
}

func dumpMsgs(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		fmt.Fprintf(&b, "\n    %s: %q", m.Role, m.Content)
	}
	return b.String()
}

type replayCall struct {
	stream *ManagedStream
	llm    *replayLLM
	log    *replayLog
	mu     sync.Mutex
	// botResponses is every reply the client was sent as the agent's line.
	botResponses []string
}

func (c *replayCall) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.botResponses...)
}

func (c *replayCall) callerSpeaking(v bool) {
	c.stream.mu.Lock()
	c.stream.vadSpeaking = v
	c.stream.mu.Unlock()
}

// utter runs one caller utterance through processUtterance, as onVADEnd would.
func (c *replayCall) utter(seq int) {
	c.stream.mu.Lock()
	c.stream.utteranceSeq = seq
	c.stream.inflightUtterances++
	c.stream.mu.Unlock()
	// The turn's clocks, which logTurnLatency anchors on: the VAD hangover ended now.
	c.stream.userSpeechEnd = time.Now()
	c.stream.lastVoicedAt = c.stream.userSpeechEnd.Add(-170 * time.Millisecond)
	c.stream.processUtterance(make([]byte, 32000), time.Second, seq)
}

// waitForCalls waits for the model to have been asked n times, then one more monitor tick (2 s)
// so anything held back — a turn resurfacing late — has had its chance to be answered too.
func (c *replayCall) waitForCalls(t *testing.T, n int) [][]Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(c.llm.calls()) < n {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(2100 * time.Millisecond)
	return c.llm.calls()
}

func newReplayCall(t *testing.T, transcripts, replies []string) *replayCall {
	t.Helper()
	c := &replayCall{llm: &replayLLM{replies: replies}, log: &replayLog{}}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 50 * time.Millisecond // production: 10 s
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithLogger(&replaySTT{texts: transcripts}, c.llm,
		&MockTTSProvider{synthesizeResult: make([]byte, 4410)}, NewRMSVAD(0.1, 100*time.Millisecond), cfg, c.log)
	c.stream = orch.NewManagedStream(context.Background(), NewConversationSession("replay-2026-09-28"))
	t.Cleanup(c.stream.Close)
	go func() {
		for ev := range c.stream.Events() {
			if ev.Type == BotResponse {
				if s, ok := ev.Data.(string); ok {
					c.mu.Lock()
					c.botResponses = append(c.botResponses, s)
					c.mu.Unlock()
				}
			}
		}
	}()
	return c
}

const (
	replayAskBooking = "Puedes, puedes reservarme la cita?"
	replayDetails    = "En el restaurante a las nueve."
	replayUnheard    = "Claro, dime el día. ¿Para cuántas personas sería la reserva?"
	replayAnswer     = "Lo siento, no gestiono reservas. Puedes escribir a contact@lokutor.com."
	replayCheckIn    = "¿Sigues ahí o necesitas algo más?"
)

func TestReplay20260928_SilenceNudgeChecksInInsteadOfReanswering(t *testing.T) {
	c := newReplayCall(t,
		[]string{replayAskBooking, replayDetails},
		[]string{replayUnheard, replayAnswer, replayCheckIn})

	// seq 3: the caller starts speaking again while its reply is being generated (32.23 s).
	c.llm.beforeReply = func(call int) {
		if call == 0 {
			c.callerSpeaking(true)
		}
	}
	c.utter(3)
	if n := len(c.log.find(discardLine)); n == 0 {
		t.Fatal("seq 3's reply was not discarded: the replay no longer reproduces the production sequence")
	}
	// seq 4: the rest of the thought, answered and played (33.84 s).
	c.callerSpeaking(false)
	c.utter(4)

	// Then silence, and the nudge on the first monitor tick past the timeout (52.06 s).
	reqs := c.waitForCalls(t, 3)
	if len(reqs) != 3 {
		t.Fatalf("model asked %d times, want 3 (seq 3, seq 4, the silence nudge) — a turn was answered twice or late", len(reqs))
	}

	// seq 4 was answered knowing only what the caller heard: the discarded reply is not in it.
	for _, m := range reqs[1] {
		if m.Role == "assistant" && m.Content == replayUnheard {
			t.Fatalf("seq 4's request holds the discarded reply as said — the caller never heard it:%s", dumpMsgs(reqs[1]))
		}
	}

	// The nudge tells the model why it is speaking; it is not the old conversation asked again.
	nudge := reqs[2]
	last, i := lastSpoken(nudge)
	if last.Role != "user" || !strings.Contains(last.Content, "said nothing") {
		t.Fatalf("the nudge re-asked the model with nothing new, so it re-answers the caller's last question "+
			"(production 52.26 s: \"No tengo acceso a reservas de restaurantes...\"). Request:%s", dumpMsgs(nudge))
	}
	if prev, _ := lastSpoken(nudge[:i]); prev.Role != "assistant" || prev.Content != replayAnswer {
		t.Fatalf("the check-in note must follow the reply the caller heard, got %+v", prev)
	}

	// The note never enters the conversation, and neither does the unheard reply.
	for _, m := range c.stream.session.GetContextCopy() {
		if strings.Contains(m.Content, "said nothing") || m.Content == replayUnheard {
			t.Fatalf("context holds %q", m.Content)
		}
	}
	for _, s := range c.sent() {
		if s == replayUnheard {
			t.Fatal("the client was sent the discarded reply as the agent's line")
		}
	}

	// One caller turn was answered, so one "turn latency" line; the nudge has its own line and
	// is not measured from seq 4's sttEnd.
	if got := c.log.find("turn latency"); len(got) != 1 {
		var cks []interface{}
		for _, l := range got {
			cks = append(cks, l.kv["ck_pre_llm_ms"])
		}
		t.Fatalf("%d turn latency lines (ck_pre_llm_ms %v), want 1: the nudge was logged as a caller turn", len(got), cks)
	}
	if got := c.log.find("bot-initiated response latency"); len(got) != 1 || got[0].kv["trigger"] != "silence_timeout" {
		t.Fatalf("want one bot-initiated line for the nudge, got %+v", got)
	}
	if got := c.log.find("Silence timeout: bot speaking unprompted"); len(got) != 1 || got[0].kv["caller_unanswered"] != false {
		t.Fatalf("want one silence-timeout line with caller_unanswered=false, got %+v", got)
	}
	if got := c.log.find("Reply not recorded: the caller resumed before any of it played"); len(got) != 1 {
		t.Fatalf("the unrecorded reply must leave one line, got %d", len(got))
	}
}

// The other half of what the nudge is for: a caller whose reply was discarded and who then said
// nothing more — the resume was a cough — is still waiting for an answer, and the nudge must give it
// rather than ask whether they are still there.
func TestReplay20260928_SilenceNudgeAnswersACallerLeftWithoutAReply(t *testing.T) {
	c := newReplayCall(t,
		[]string{replayAskBooking},
		[]string{replayUnheard, replayAnswer})

	c.llm.beforeReply = func(call int) {
		if call == 0 {
			c.callerSpeaking(true)
		}
	}
	c.utter(3)
	if n := len(c.log.find(discardLine)); n == 0 {
		t.Fatal("seq 3's reply was not discarded")
	}
	c.callerSpeaking(false) // nothing came of it

	reqs := c.waitForCalls(t, 2)
	if len(reqs) != 2 {
		t.Fatalf("model asked %d times, want 2 (seq 3, the nudge)", len(reqs))
	}
	last, _ := lastSpoken(reqs[1])
	if last.Role != "user" || last.Content != replayAskBooking {
		t.Fatalf("the nudge must put the unanswered question to the model, not a check-in or a reply "+
			"the caller never heard. Request:%s", dumpMsgs(reqs[1]))
	}
	ctx := c.stream.session.GetContextCopy()
	if m, _ := lastSpoken(ctx); m.Role != "assistant" || m.Content != replayAnswer {
		t.Fatalf("the caller's question must end up answered in context, got %+v", m)
	}
	if got := c.log.find("Silence timeout: bot speaking unprompted"); len(got) != 1 || got[0].kv["caller_unanswered"] != true {
		t.Fatalf("want one silence-timeout line with caller_unanswered=true, got %+v", got)
	}
}
