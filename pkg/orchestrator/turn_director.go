package orchestrator

import (
	"context"
	"strings"
	"time"
)

// TurnDirector (opt-in: a host sets one with SetTurnDirector) decides, before the model answers a caller's turn, what that
// turn changes outside the conversation: which step of a workflow it leaves the call in, what details it gave. The model
// that talks then only talks. Asked to do both, a model without thinking wrote its bookkeeping into what it said ("Wait, I
// need to save it first", in English, on Spanish calls: Claude Haiku 5.5, 2026-11-25), and forgot half of it.
//
// Decide has no effects and is asked while the pause is still being measured, beside the guess at the reply; Apply, once the
// turn is committed, reports whether anything changed. When nothing did the guess is used as it is, so a turn that changes
// nothing waits only for what Decide takes longer than the reply; when something did, the reply is asked for again.
type TurnDirector interface {
	Decide(ctx context.Context, words string) interface{} // nil: nothing to decide, or it could not
	Apply(decision interface{}) (changed bool)
}

// turnDirectorBudget is the longest a turn waits for a decision.
const turnDirectorBudget = 2 * time.Second

// SetTurnDirector sets the director of this orchestrator's calls; nil for none.
func (o *Orchestrator) SetTurnDirector(d TurnDirector) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.director = d
}

func (o *Orchestrator) turnDirector() TurnDirector {
	if o == nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.director
}

type directorRun struct {
	words    string
	done     chan struct{}
	decision interface{}
	ms       int64
}

// directTurn asks about words, unless it is being asked already.
func (ms *ManagedStream) directTurn(d TurnDirector, words string) *directorRun {
	ms.mu.Lock()
	if r := ms.dirRun; r != nil && sameUtterance(r.words, words) {
		ms.mu.Unlock()
		return r
	}
	r := &directorRun{words: words, done: make(chan struct{})}
	ms.dirRun = r
	ms.mu.Unlock()
	go func() {
		defer close(r.done)
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(ms.ctx, turnDirectorBudget)
		defer cancel()
		r.decision = d.Decide(ctx, words)
		r.ms = time.Since(t0).Milliseconds()
	}()
	return r
}

// directed applies the director's decision about a committed turn and reports whether it changed anything.
func (ms *ManagedStream) directed(ctx context.Context, words string) bool {
	d := ms.orch.turnDirector()
	if d == nil || strings.TrimSpace(words) == "" {
		return false
	}
	r := ms.directTurn(d, words)
	t0 := time.Now()
	select {
	case <-r.done:
	case <-time.After(turnDirectorBudget):
	case <-ctx.Done():
	}
	ms.mu.Lock()
	ms.dirRun = nil
	ms.mu.Unlock()
	select {
	case <-r.done:
	default:
		ms.logger.Warn("Turn director: no decision in time", "waited_ms", time.Since(t0).Milliseconds())
		return false
	}
	changed := r.decision != nil && d.Apply(r.decision)
	ms.logger.Info("Turn director", "ms", r.ms, "waited_ms", time.Since(t0).Milliseconds(), "decided", r.decision != nil, "changed", changed)
	return changed
}
