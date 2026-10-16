package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowStreamLLM streams a scripted reply piece by piece, gap apart, and counts requests. After the first request it
// answers from later (a round after tools).
type slowStreamLLM struct {
	mu      sync.Mutex
	streams int
	pieces  []string
	calls   []ToolCallEventData
	gap     time.Duration
	failAt  int // first request fails before any output when > 0
	later   []string
}

func (m *slowStreamLLM) Name() string { return "slowStreamLLM" }

func (m *slowStreamLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "", nil
}

func (m *slowStreamLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	m.mu.Lock()
	n := m.streams
	m.streams++
	m.mu.Unlock()
	if n > 0 {
		var all strings.Builder
		for _, p := range m.later {
			all.WriteString(p)
			if err := onChunk(p); err != nil {
				return "", err
			}
		}
		return all.String(), nil
	}
	if m.failAt > 0 {
		return "", errors.New("provider down")
	}
	var all strings.Builder
	for _, p := range m.pieces {
		select {
		case <-time.After(m.gap):
		case <-ctx.Done():
			return all.String(), ctx.Err()
		}
		all.WriteString(p)
		if err := onChunk(p); err != nil {
			return "", err
		}
	}
	for _, tc := range m.calls {
		if err := onToolCall(tc); err != nil {
			return "", err
		}
	}
	return all.String(), nil
}

// timedTTS records when each sentence was asked for.
type timedTTS struct {
	countingTTS
	at []time.Time
}

func (o *timedTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	o.mu.Lock()
	o.at = append(o.at, time.Now())
	o.mu.Unlock()
	return o.countingTTS.StreamSynthesize(ctx, text, voice, lang, onChunk)
}

func waitSaid(t *testing.T, tts *countingTTS, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		tts.mu.Lock()
		joined := strings.Join(tts.said, " | ")
		tts.mu.Unlock()
		if strings.Contains(joined, want) || time.Now().After(deadline) {
			return joined
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A turn whose transcript is a streaming run's takes the stream over while it is still streaming: its first sentence is
// spoken before the reply has finished arriving, and the question is asked once.
func TestTurnTakesOverAStreamingSpeculativeRun(t *testing.T) {
	llm := &slowStreamLLM{pieces: []string{"Claro, abrimos a las nueve. ", "Y cerramos ", "a las seis."}, gap: 150 * time.Millisecond}
	tts := &timedTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("spec-stream"))
	defer ms.Close()

	ms.speculator.StartFromTranscript(context.Background(), orch, "¿a qué hora abrís?", nil, nil)
	time.Sleep(200 * time.Millisecond) // the first sentence has arrived, the rest has not
	round, ok := ms.speculator.TakeStream("¿A qué hora abrís?")
	if !ok {
		t.Fatal("the matching stream was not taken")
	}
	ms.specStreamRound = round
	started := time.Now()
	ms.runLLMAndTTS(context.Background(), "¿A qué hora abrís?")
	joined := waitSaid(t, &tts.countingTTS, "seis")
	tts.mu.Lock()
	firstAt := tts.at[0]
	tts.mu.Unlock()
	if !strings.Contains(joined, "nueve") || !strings.Contains(joined, "seis") {
		t.Fatalf("reply not spoken whole: %s", joined)
	}
	if strings.Count(joined, "nueve") != 1 {
		t.Errorf("first sentence said more than once: %s", joined)
	}
	if d := firstAt.Sub(started); d > 100*time.Millisecond {
		t.Errorf("first sentence waited %v for the rest of the stream", d)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if llm.streams != 1 {
		t.Errorf("the model was asked %d times, want 1", llm.streams)
	}
}

// Tool calls that arrive through a taken stream run once, and the round after them is asked as usual.
func TestTakenStreamRunsItsToolCallsOnce(t *testing.T) {
	llm := &slowStreamLLM{pieces: []string{"Un momento, lo miro."}, calls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}},
		gap: 20 * time.Millisecond, later: []string{"El lunes a las diez está libre."}}
	tts := &countingTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	var ran int
	var mu sync.Mutex
	orch.RegisterTool("lookup", func(args string) (string, error) { mu.Lock(); ran++; mu.Unlock(); return `{"ok":true}`, nil })
	session := NewConversationSession("spec-stream-tools")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	ms.speculator.StartFromTranscript(context.Background(), orch, "mira el lunes", nil, session.GetTools())
	round, ok := ms.speculator.TakeStream("Mira el lunes.")
	if !ok {
		t.Fatal("not taken")
	}
	ms.specStreamRound = round
	ms.runLLMAndTTS(context.Background(), "Mira el lunes.")
	joined := waitSaid(t, tts, "diez")
	if strings.Count(joined, "lo miro") != 1 || !strings.Contains(joined, "diez") {
		t.Fatalf("spoken: %s", joined)
	}
	mu.Lock()
	defer mu.Unlock()
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if ran != 1 || llm.streams != 2 {
		t.Errorf("tool ran %d times, model asked %d times; want 1 and 2", ran, llm.streams)
	}
}

// A stream that failed before delivering anything costs the turn a normal request, not the reply.
func TestFailedStreamFallsBackToTheProvider(t *testing.T) {
	llm := &slowStreamLLM{failAt: 1, later: []string{"Abrimos a las nueve."}}
	tts := &countingTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("spec-stream-fail"))
	defer ms.Close()

	ms.speculator.StartFromTranscript(context.Background(), orch, "¿a qué hora abrís?", nil, nil)
	round, ok := ms.speculator.TakeStream("¿A qué hora abrís?")
	if !ok {
		t.Fatal("not taken")
	}
	ms.specStreamRound = round
	ms.runLLMAndTTS(context.Background(), "¿A qué hora abrís?")
	if joined := waitSaid(t, tts, "nueve"); !strings.Contains(joined, "nueve") {
		t.Fatalf("no reply after the stream failed: %s", joined)
	}
}

// A stream for other words is not taken, and the run is left as it was.
func TestStreamForOtherWordsIsNotTaken(t *testing.T) {
	llm := &slowStreamLLM{pieces: []string{"Hola."}, gap: 10 * time.Millisecond}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, &countingTTS{}, nil, DefaultConfig(), &NoOpLogger{})
	se := NewSpeculativeExecutor(0)
	se.StartFromTranscript(context.Background(), orch, "hola", nil, nil)
	if _, ok := se.TakeStream("hola, quería una cita"); ok {
		t.Fatal("a stream for other words was taken")
	}
	if se.State() == SpecIdle {
		t.Fatal("the run was dropped by a refused take")
	}
	se.Cancel()
}
