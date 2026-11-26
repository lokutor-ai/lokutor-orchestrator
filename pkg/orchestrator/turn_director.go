package orchestrator

import (
	"context"
	"encoding/json"
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

// TurnPreview is what the model would be asked with once a decision is applied: what SetSystemPrompt and SetStepPrompt
// would be given, and the tools offered.
type TurnPreview struct {
	SystemPrompt string
	StepPrompt   string
	Tools        []Tool
}

// TurnPreviewer is a TurnDirector that can tell, without changing anything, what Apply would make the next request be.
// Preview is nil when it cannot (the move runs something on the way, or puts the call on another model) or when the
// decision changes nothing the model is asked with.
//
// With it, a decision that arrives while the pause is still being measured asks for the reply again at once, from the
// step the call is going to, instead of after the turn is committed and the guess thrown away (an early re-ask). The
// committed turn uses that reply only when Apply left the session exactly as previewed. It is no extra request: the
// guess it replaces would have been thrown away at the commit. What it wins is the time between the decision and the
// commit, so it shows only when the turn's end takes longer to settle than the director to answer: replayed from a
// worker pod with Claude Haiku 5.5 (lokutor_tts finance/measurements/flow_latency_2026-11-26/, 32 step changes), the
// director answers 518 ms after the seed (p50) and the new reply's first token comes 488 ms after it is asked; with the
// turn committed 700 ms after the seed a step change speaks 156 ms sooner (p50), at 450 ms or less nothing.
type TurnPreviewer interface {
	Preview(decision interface{}) *TurnPreview
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
	// claimed: the committed turn is waiting for this run (directed); a decision arriving after it is applied there.
	claimed bool
	// early is the re-ask started when the decision arrived before the turn was committed (ms.mu).
	early *earlyReask
}

// earlyReask is a guess asked again, before commit, as a decision would leave the call.
type earlyReask struct {
	at      time.Time
	preview *TurnPreview
}

// reaskSeed is what the latest hangover guess was asked with, for asking it again from another step.
type reaskSeed struct {
	words         string
	history, tail []Message
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
		ms.reaskEarly(d, r)
	}()
	return r
}

// reaskEarly asks the guess again from where the decision moves the call, when it came before the turn was committed.
func (ms *ManagedStream) reaskEarly(d TurnDirector, r *directorRun) {
	p, ok := d.(TurnPreviewer)
	if !ok || r.decision == nil || ms.speculator == nil || ms.orch == nil || !ms.orch.config.SpeculativeLLM {
		return
	}
	ms.mu.Lock()
	seed := ms.reaskSeed
	open := ms.dirRun == r && !r.claimed && seed != nil && sameUtterance(seed.words, r.words)
	ms.mu.Unlock()
	if !open {
		return
	}
	preview := p.Preview(r.decision)
	if preview == nil {
		return
	}
	history := ms.session.asAfter(seed.history, preview.SystemPrompt, preview.StepPrompt)
	ms.mu.Lock()
	if ms.dirRun != r || r.claimed {
		ms.mu.Unlock()
		return // committed meanwhile: the turn asks after Apply
	}
	r.early = &earlyReask{at: time.Now(), preview: preview}
	ms.mu.Unlock()
	ms.prerender.discard()
	ms.speculator.RestartFromTranscriptWithTail(ms.ctx, ms.orch, seed.words, history, seed.tail, preview.Tools)
	ms.logger.Info("Turn director: reply asked again before the turn ended", "run", ms.speculator.RunID(), "decide_ms", r.ms)
}

// directedOutcome is what a committed turn does about its reply after the director.
type directedOutcome int

const (
	directedNone    directedOutcome = iota // nothing changed: the guess stands
	directedChanged                        // the call changed: the reply is asked again now
	directedEarly                          // the call changed as previewed: the guess asked again early stands
)

// directed applies the director's decision about a committed turn and says what the turn does about its reply.
func (ms *ManagedStream) directed(ctx context.Context, words string) directedOutcome {
	ms.mu.Lock()
	ms.reaskKind, ms.reaskEarlyMs = "", 0
	ms.mu.Unlock()
	d := ms.orch.turnDirector()
	if d == nil || strings.TrimSpace(words) == "" {
		return directedNone
	}
	r := ms.directTurn(d, words)
	ms.mu.Lock()
	r.claimed = true
	ms.mu.Unlock()
	t0 := time.Now()
	select {
	case <-r.done:
	case <-time.After(turnDirectorBudget):
	case <-ctx.Done():
	}
	ms.mu.Lock()
	ms.dirRun = nil
	ms.reaskSeed = nil
	early := r.early
	ms.mu.Unlock()
	select {
	case <-r.done:
	default:
		ms.logger.Warn("Turn director: no decision in time", "waited_ms", time.Since(t0).Milliseconds())
		if early != nil {
			return directedChanged // cannot be: early is set before done; kept so a guess never outlives its decision
		}
		return directedNone
	}
	changed := r.decision != nil && d.Apply(r.decision)
	out, kind, earlyBy := directedNone, "", int64(0)
	switch {
	case early != nil && changed && ms.session.promptIs(early.preview.SystemPrompt, early.preview.StepPrompt) &&
		sameTools(ms.session.GetTools(), early.preview.Tools):
		out, kind, earlyBy = directedEarly, "early", time.Since(early.at).Milliseconds()
	case early != nil:
		out, kind = directedChanged, "early_discarded" // asked from a step the call is not in
	case changed:
		out, kind = directedChanged, "late"
	}
	ms.mu.Lock()
	ms.reaskKind, ms.reaskEarlyMs = kind, earlyBy
	ms.mu.Unlock()
	ms.logger.Info("Turn director", "ms", r.ms, "waited_ms", time.Since(t0).Milliseconds(), "decided", r.decision != nil, "changed", changed,
		"reask", kind, "reask_early_ms", earlyBy)
	return out
}

// sameTools reports whether two tool lists are the same, as a request would carry them.
func sameTools(a, b []Tool) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
