package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDirector struct {
	slow    time.Duration
	change  bool
	asked   atomic.Int32
	applied atomic.Int32
	orch    *Orchestrator
	session *ConversationSession
}

func (f *fakeDirector) Decide(ctx context.Context, words string) interface{} {
	f.asked.Add(1)
	select {
	case <-time.After(f.slow):
	case <-ctx.Done():
		return nil
	}
	return words
}
func (f *fakeDirector) Apply(d interface{}) bool {
	f.applied.Add(1)
	if f.change {
		f.orch.SetSystemPrompt(f.session, "STEP TWO")
	}
	return f.change
}

// The director's decision is in place before the model is asked, and is asked for once a turn.
func TestTurnDirectorDecidesBeforeTheReply(t *testing.T) {
	llm := &stepLLM{name: "talker", say: "Le llamaba por unos tickets."}
	ms, orch := listenStream(t, &piecesSTT{pieces: []string{"Ferretería Sol"}, eou: 0.95}, llm, fixedAudioTTS{})
	orch.SetSystemPrompt(ms.session, "STEP ONE")
	d := &fakeDirector{slow: 150 * time.Millisecond, change: true, orch: orch, session: ms.session}
	orch.SetTurnDirector(d)
	piece(ms, 600*time.Millisecond)
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if llm.asked != 1 || d.asked.Load() != 1 || d.applied.Load() != 1 {
		t.Fatalf("talker asked %d, director asked %d, applied %d", llm.asked, d.asked.Load(), d.applied.Load())
	}
	if !containsStr(llm.seen[0], "STEP TWO") {
		t.Fatalf("answered from the step before: %q", llm.seen[0])
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
