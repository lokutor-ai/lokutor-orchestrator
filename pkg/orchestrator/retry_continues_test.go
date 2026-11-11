package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// chunkedLLM streams each scripted reply in the pieces given, as a provider does, and keeps every request.
type chunkedLLM struct {
	mu       sync.Mutex
	script   [][]string
	requests [][]Message
	efforts  []string
}

func (m *chunkedLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "", nil
}

func (m *chunkedLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	m.mu.Lock()
	m.requests = append(m.requests, append([]Message(nil), messages...))
	m.efforts = append(m.efforts, ReasoningEffortFrom(ctx))
	n := len(m.requests)
	m.mu.Unlock()
	if n > len(m.script) {
		return "", nil
	}
	var all strings.Builder
	for _, c := range m.script[n-1] {
		all.WriteString(c)
		if err := onChunk(c); err != nil {
			return "", err
		}
	}
	return all.String(), nil
}

func (m *chunkedLLM) Name() string { return "chunkedLLM" }

// countingTTS records every piece of text it is asked to say.
type countingTTS struct {
	mu   sync.Mutex
	said []string
}

func (c *countingTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	c.mu.Lock()
	c.said = append(c.said, text)
	c.mu.Unlock()
	return []byte{1, 2, 3}, nil
}

func (c *countingTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	c.mu.Lock()
	c.said = append(c.said, text)
	c.mu.Unlock()
	return onChunk([]byte{1, 2, 3})
}

func (c *countingTTS) Abort() error { return nil }

func (c *countingTTS) Name() string { return "countingTTS" }

// A reply stopped at a written-out tool call after it had begun to be spoken: the second try is told what the caller
// already heard, asked at "medium", and the caller hears the goodbye once. On 2026-10-15 the second try started over
// and the goodbye was spoken twice.
func TestRetryAfterWrittenOutCallContinuesFromWhatWasSaid(t *testing.T) {
	llm := &chunkedLLM{script: [][]string{
		{"Lo siento, Manolo. ", "Que tengas un buen día. ", `{"name": "end_call", "arguments": {}}`},
		{""},
	}}
	tts := &countingTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "déjame en paz"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	session := NewConversationSession("retry-continues")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "end_call"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(context.Background(), "no quiero ninguna, déjame en paz"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish")
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.requests) != 2 {
		t.Fatalf("the model was asked %d times, want 2", len(llm.requests))
	}
	second := llm.requests[1]
	last := second[len(second)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "Que tengas un buen día.") || !strings.Contains(last.Content, "Lo siento, Manolo.") {
		t.Fatalf("the second try was not told what the caller heard; its request ends with %s: %q", last.Role, last.Content)
	}
	if llm.efforts[1] != "medium" {
		t.Errorf("second try asked at %q, want medium", llm.efforts[1])
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	joined := strings.Join(tts.said, " | ")
	if n := strings.Count(joined, "buen día"); n != 1 {
		t.Errorf("the goodbye was spoken %d times: %s", n, joined)
	}
}

// Words before a tool call are the acknowledgment and are said once, whether their sentence was already flushed to TTS
// when the call arrived or is still an unfinished tail. On 2026-10-16 both were said twice: the whole reply was spoken
// again as the filler, and the tail once more at the end of the turn.
func TestPreToolWordsSpokenOnce(t *testing.T) {
	for _, pre := range []string{"Un momento, lo miro.", "Un momento, lo miro"} {
		llm := &MockStreamingLLM{responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{content: pre, toolCalls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}}},
			{content: "Ya está.", toolCalls: nil},
		}}
		tts := &countingTTS{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
		orch.RegisterTool("lookup", func(args string) (string, error) { return `{"ok":true}`, nil })
		session := NewConversationSession("pre-tool-once")
		session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
		ms := orch.NewManagedStream(context.Background(), session)
		ms.runLLMAndTTS(context.Background(), "mira mi cita")
		deadline := time.Now().Add(3 * time.Second)
		for {
			tts.mu.Lock()
			joined := strings.Join(tts.said, " | ")
			tts.mu.Unlock()
			if strings.Contains(joined, "Ya está") || time.Now().After(deadline) {
				if n := strings.Count(joined, "lo miro"); n != 1 {
					t.Errorf("%q: the words before the call were said %d times: %s", pre, n, joined)
				}
				if !strings.Contains(joined, "Ya está") {
					t.Errorf("%q: the reply after the call was not said: %s", pre, joined)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		ms.Close()
	}
}

// openerTTS records, per sentence, whether its synthesis was marked as the turn's first sentence.
type openerTTS struct {
	countingTTS
	opener []bool
}

func (o *openerTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	o.mu.Lock()
	o.opener = append(o.opener, TurnOpener(ctx))
	o.mu.Unlock()
	return o.countingTTS.StreamSynthesize(ctx, text, voice, lang, onChunk)
}

// Only a turn's first sentence is marked TurnOpener: the caller waits on it, the rest synthesize while it plays.
func TestOnlyTheTurnsFirstSentenceIsTheOpener(t *testing.T) {
	llm := &chunkedLLM{script: [][]string{{"Claro, te ayudo. ", "Dime tu nombre. ", "Y tu teléfono."}}}
	tts := &openerTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("opener"))
	defer ms.Close()
	ms.runLLMAndTTS(context.Background(), "quiero una cita")
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if len(tts.opener) < 2 || !tts.opener[0] {
		t.Fatalf("first sentence not marked as the opener: %v (%v)", tts.opener, tts.said)
	}
	for i, o := range tts.opener[1:] {
		if o {
			t.Errorf("sentence %d marked as the opener: %v (%v)", i+2, tts.opener, tts.said)
		}
	}
}

// The model's own words before its calls are recorded with the calls, since the caller heard them; a
// model that cannot see them says them again after the result (Claude Haiku 5.5, 2026-11-11). The
// platform's filler is not the model's and is not recorded.
func TestPreToolWordsRecordedWithTheCalls(t *testing.T) {
	for _, tc := range []struct{ pre, want string }{
		{"Vale, le reservo el viernes a las nueve y media.", "Vale, le reservo el viernes a las nueve y media."},
		{"", ""},
	} {
		llm := &MockStreamingLLM{responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{content: tc.pre, toolCalls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}}},
			{content: "Hecho.", toolCalls: nil},
		}}
		tts := &countingTTS{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
		orch.RegisterTool("lookup", func(args string) (string, error) { return `{"ok":true}`, nil })
		session := NewConversationSession("pre-tool-recorded")
		session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
		ms := orch.NewManagedStream(context.Background(), session)
		ms.runLLMAndTTS(context.Background(), "el viernes a las nueve y media")
		found := false
		for _, m := range session.GetContextCopy() {
			if m.Role == "assistant" && m.ToolCalls != nil {
				found = true
				if m.Content != tc.want {
					t.Errorf("pre %q: the message carrying the call has %q, want %q", tc.pre, m.Content, tc.want)
				}
			}
		}
		if !found {
			t.Errorf("pre %q: no assistant message carries the call", tc.pre)
		}
		ms.Close()
	}
}

// A goodbye said with a final tool (end_call) ends the turn: the model is not asked again after the result,
// where it said the goodbye a second time. Said without words, the call is answered as before, so the model
// can say its goodbye then.
func TestFinalToolEndsTheTurnWithItsWords(t *testing.T) {
	for _, tc := range []struct {
		pre        string
		wantRounds int
		wantSaid   string
	}{
		{"Hasta el viernes, Carmen. Adiós.", 1, "Hasta el viernes"},
		{"", 2, "Adiós, que vaya bien"},
	} {
		llm := &MockStreamingLLM{responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{content: tc.pre, toolCalls: []ToolCallEventData{{Name: "end_call", Arguments: `{"reason":"user_ended"}`, CallID: "e1"}}},
			{content: "Adiós, que vaya bien.", toolCalls: nil},
		}}
		tts := &countingTTS{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
		orch.RegisterTool("end_call", func(args string) (string, error) { return `{"ok":true}`, nil })
		orch.SetFinalTools("end_call")
		session := NewConversationSession("final-tool")
		session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "end_call"}}})
		ms := orch.NewManagedStream(context.Background(), session)
		ms.runLLMAndTTS(context.Background(), "gracias, adiós")
		time.Sleep(300 * time.Millisecond) // a reply after the result is asked for in its own goroutine
		tts.mu.Lock()
		joined := strings.Join(tts.said, " | ")
		tts.mu.Unlock()
		if llm.callCount != tc.wantRounds {
			t.Errorf("pre %q: the model was asked %d times, want %d (%s)", tc.pre, llm.callCount, tc.wantRounds, joined)
		}
		if !strings.Contains(joined, tc.wantSaid) || strings.Count(joined, "Adiós") > 1 {
			t.Errorf("pre %q: said %s", tc.pre, joined)
		}
		ms.Close()
	}
}
