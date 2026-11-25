package orchestrator

import (
	"context"
	"strings"
	"time"
)

// DictationJudge (opt-in: a host sets one with SetDictationJudge, and says when it is active) tells a piece of something
// the caller is dictating from any other turn. Neither the end-of-turn heads nor the length of a pause can: "Librería." and
// "Central." read as finished turns (0.87-0.99), as "Sí." does. Asking the agent's own model on every piece made it talk over
// the dictation ("Vale, central. ¿Y después?"), whatever its instructions said. So the pieces never reach it: while the
// judge says there is more, the turn is held silently (Listening), and the model is asked once, with all of it.
//
// It is asked while the pause is still being measured (from the speculative transcript), so an ordinary turn waits for it
// little or nothing. No answer in time counts as an ordinary turn, or as the listening there already was.
type DictationJudge interface {
	Active() bool                                                 // dictation is expected now
	Judge(ctx context.Context, t DictationTurn) DictationTurnKind // "" when it cannot tell
	Listening() *Listening                                        // how to wait while there is more
}

// DictationTurn is what the judge is shown: the agent's last words, the caller's since, and the last piece of them.
type DictationTurn struct {
	AgentLine, Words, LastPiece string
	Dictating                   bool // they were in the middle of it already
}

type DictationTurnKind string

const (
	DictationMore  DictationTurnKind = "more"
	DictationWhole DictationTurnKind = "whole"
	DictationOther DictationTurnKind = "other"
)

// dictationJudgeBudget is the longest a turn waits for a verdict.
const dictationJudgeBudget = 800 * time.Millisecond

// SetDictationJudge sets the judge of this orchestrator's calls; nil for none.
func (o *Orchestrator) SetDictationJudge(j DictationJudge) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dictation = j
}

func (o *Orchestrator) dictationJudge() DictationJudge {
	if o == nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.dictation
}

// dictationRun is one request to the judge, kept for the turn whose words it was asked about.
type dictationRun struct {
	words   string
	done    chan struct{}
	verdict DictationTurnKind
	ms      int64
}

// activeJudge is the judge when it has something to say about this stream now.
func (ms *ManagedStream) activeJudge() DictationJudge {
	j := ms.orch.dictationJudge()
	if j == nil || (!j.Active() && ms.listeningTo() == nil) {
		return nil
	}
	return j
}

// judgeDictation asks about words, unless it is being asked already.
func (ms *ManagedStream) judgeDictation(j DictationJudge, words, lastPiece string) *dictationRun {
	ms.mu.Lock()
	if r := ms.dictRun; r != nil && sameUtterance(r.words, words) {
		ms.mu.Unlock()
		return r
	}
	r := &dictationRun{words: words, done: make(chan struct{})}
	ms.dictRun = r
	ms.mu.Unlock()
	turn := DictationTurn{AgentLine: ms.lastAgentLine(), Words: strings.TrimSpace(ms.unansweredWords() + " " + words), LastPiece: lastPiece, Dictating: ms.IsListening()}
	go func() {
		defer close(r.done)
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(ms.ctx, 2*time.Second)
		defer cancel()
		r.verdict = j.Judge(ctx, turn)
		r.ms = time.Since(t0).Milliseconds()
	}()
	return r
}

// verdict is the run's, waited for at most budget.
func (r *dictationRun) wait(ctx context.Context, budget time.Duration) DictationTurnKind {
	select {
	case <-r.done:
		return r.verdict
	case <-time.After(budget):
	case <-ctx.Done():
	}
	return ""
}

// unansweredWords are the caller's turns the agent has not answered, before this one.
func (ms *ManagedStream) unansweredWords() string {
	h := ms.session.GetContextCopy()
	var out []string
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "system" || h[i].Role == "tool" {
			continue
		}
		if h[i].Role != "user" || h[i].Content == OpeningTrigger {
			break
		}
		out = append([]string{h[i].Content}, out...)
	}
	return strings.Join(out, " ")
}

// dictationVerdict is what the judge says of a turn's words, and the listening the turn waits with ("" and what there was
// when it does not say).
func (ms *ManagedStream) dictationVerdict(ctx context.Context, listen *Listening, words, lastPiece string) (*Listening, DictationTurnKind) {
	j := ms.activeJudge()
	if j == nil || strings.TrimSpace(words) == "" {
		return listen, ""
	}
	r := ms.judgeDictation(j, words, lastPiece)
	if d := ms.orch.turnDirector(); d != nil && listen == nil {
		ms.directTurn(d, words) // most turns here are not a piece: decided at the same time, not one after the other
	}
	v := r.wait(ctx, dictationJudgeBudget)
	ms.logger.Info("Dictation", "verdict", string(v), "ms", r.ms, "listening", listen != nil, "words", ContentForLog(words))
	switch v {
	case DictationMore:
		if listen == nil {
			listen = j.Listening()
			ms.Listen(listen)
		}
	case DictationOther:
		listen = nil // for this turn: the caller is talking to the agent
	}
	return listen, v
}
