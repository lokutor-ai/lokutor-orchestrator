package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// reaskLLM records each request's step message and answers after a short while.
type reaskLLM struct {
	mu    sync.Mutex
	steps []string
}

func (l *reaskLLM) Name() string { return "talker" }
func (l *reaskLLM) Complete(ctx context.Context, m []Message, t []Tool) (string, error) {
	return l.StreamComplete(ctx, m, t, nil, nil)
}
func (l *reaskLLM) StreamComplete(ctx context.Context, m []Message, t []Tool, onChunk func(string) error, _ func(ToolCallEventData) error) (string, error) {
	step := ""
	for i, x := range m {
		if i > 0 && x.Role == "system" {
			step = x.Content
		}
	}
	l.mu.Lock()
	l.steps = append(l.steps, step)
	l.mu.Unlock()
	select {
	case <-time.After(30 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	reply := "Respuesta desde " + step
	if onChunk != nil {
		_ = onChunk(reply)
	}
	return reply, nil
}
func (l *reaskLLM) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

// previewDirector moves the call to step "S2"; apply is what Apply really leaves (S2 as previewed, or else).
type previewDirector struct {
	slow    time.Duration
	preview *TurnPreview
	apply   string
	orch    *Orchestrator
	session *ConversationSession
}

func (d *previewDirector) Decide(ctx context.Context, words string) interface{} {
	time.Sleep(d.slow)
	return "move"
}
func (d *previewDirector) Apply(interface{}) bool {
	if d.apply == "" {
		return false
	}
	d.session.SetStepPrompt(d.apply)
	return true
}
func (d *previewDirector) Preview(interface{}) *TurnPreview { return d.preview }

func reaskStream(t *testing.T, llm LLMProvider) (*ManagedStream, *Orchestrator) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SpeculativeLLM = true
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(nil, llm, nil, nil, cfg, &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("reask"))
	t.Cleanup(ms.Close)
	go func() {
		for range ms.Events() {
		}
	}()
	orch.SetSystemPrompt(ms.session, "AGENT")
	ms.session.SetStepPrompt("S1")
	return ms, orch
}

// seedGuess does what the hangover seed does: records what the guess is asked with, asks the director, asks the guess.
func seedGuess(ms *ManagedStream, d TurnDirector, words string) *directorRun {
	history := ms.session.GetContextCopy()
	ms.mu.Lock()
	ms.reaskSeed = &reaskSeed{words: words, history: history}
	ms.mu.Unlock()
	r := ms.directTurn(d, words)
	ms.speculator.StartFromTranscriptWithTail(ms.ctx, ms.orch, words, history, nil, ms.session.GetTools())
	return r
}

func TestEarlyReask(t *testing.T) {
	cases := []struct {
		name      string
		commitAt  time.Duration // when the turn is committed, after the seed
		slow      time.Duration // how long the director takes
		apply     string
		preview   *TurnPreview
		want      directedOutcome
		kind      string
		reaskedS2 bool // a guess was asked from S2 before the commit
	}{
		{"decided before the commit, applied as previewed", 120 * time.Millisecond, 20 * time.Millisecond, "S2",
			&TurnPreview{SystemPrompt: "AGENT", StepPrompt: "S2"}, directedEarly, "early", true},
		{"decided before the commit, applied otherwise", 120 * time.Millisecond, 20 * time.Millisecond, "S3",
			&TurnPreview{SystemPrompt: "AGENT", StepPrompt: "S2"}, directedChanged, "early_discarded", true},
		{"decided after the commit", 10 * time.Millisecond, 120 * time.Millisecond, "S2",
			&TurnPreview{SystemPrompt: "AGENT", StepPrompt: "S2"}, directedChanged, "late", false},
		{"no change", 120 * time.Millisecond, 20 * time.Millisecond, "", nil, directedNone, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			llm := &reaskLLM{}
			ms, orch := reaskStream(t, llm)
			d := &previewDirector{slow: c.slow, preview: c.preview, apply: c.apply, orch: orch, session: ms.session}
			orch.SetTurnDirector(d)
			seedGuess(ms, d, "sí, soy yo")
			time.Sleep(c.commitAt)
			ms.session.AddMessage("user", "sí, soy yo")
			got := ms.directed(context.Background(), "sí, soy yo")
			ms.mu.Lock()
			kind, early := ms.reaskKind, ms.reaskEarlyMs
			ms.mu.Unlock()
			if got != c.want || kind != c.kind {
				t.Fatalf("outcome %d %q, want %d %q", got, kind, c.want, c.kind)
			}
			if c.kind == "early" && early <= 0 {
				t.Fatalf("early by %d ms", early)
			}
			asked := strings.Join(llm.asked(), ",")
			if c.reaskedS2 != strings.Contains(asked, "S2") {
				t.Fatalf("guesses asked from %s", asked)
			}
			if got == directedEarly {
				reply, ok := ms.speculator.Await(context.Background(), "sí, soy yo")
				if !ok || reply != "Respuesta desde S2" {
					t.Fatalf("the turn gets %q %v", reply, ok)
				}
			}
		})
	}
}

// The caller goes on: the early guess is for words the turn no longer has, and the next seed replaces it.
func TestEarlyReaskCallerResumes(t *testing.T) {
	llm := &reaskLLM{}
	ms, orch := reaskStream(t, llm)
	d := &previewDirector{slow: 10 * time.Millisecond, preview: &TurnPreview{SystemPrompt: "AGENT", StepPrompt: "S2"}, apply: "S2", orch: orch, session: ms.session}
	orch.SetTurnDirector(d)
	seedGuess(ms, d, "sí")
	time.Sleep(60 * time.Millisecond)
	seedGuess(ms, d, "sí, pero antes una pregunta")
	time.Sleep(60 * time.Millisecond)
	ms.session.AddMessage("user", "sí, pero antes una pregunta")
	if got := ms.directed(context.Background(), "sí, pero antes una pregunta"); got != directedEarly {
		t.Fatalf("outcome %d", got)
	}
	if _, ok := ms.speculator.Await(context.Background(), "sí"); ok {
		t.Fatal("the first words' guess is still there")
	}
	if reply, ok := ms.speculator.Await(context.Background(), "sí, pero antes una pregunta"); !ok || reply != "Respuesta desde S2" {
		t.Fatalf("%q %v", reply, ok)
	}
}
