package orchestrator

import (
	"context"
	"strings"
	"time"
)

// Listening (opt-in: only a host that calls Listen turns it on). Somebody who dictates an address or a number says it in
// pieces with pauses of seconds between them ("librería… central… arroba… gmail… punto com"), and every piece reads as a
// finished turn: the recogniser's end-of-turn head gave such pieces 0.8-1.0 (60 of them, Spanish, telephone, 2026-11-25),
// so each was answered at once. While the host has the stream listening, a turn waits Patience for more whatever the heads
// say, or DoneWait once Done says its words are the whole of it, and what comes in the meantime joins it: the pieces are
// transcribed together, as a held turn's continuation always is. A short word is never dropped as a filler, as noise or as
// too short to interrupt, the agent says nothing it was not asked for (no "let me think", no backchannel, no guess at the
// reply), and the silence check-in waits Silence.
//
// It ends by itself when the agent speaks in a later turn. Ordinary turns, and every stream whose host never calls Listen,
// keep the timing they had.

// Listening is what a stream waits for while it listens.
type Listening struct {
	Patience time.Duration                // how long a turn waits for more
	Done     func(transcript string) bool // the words are the whole of what is being dictated; nil: nobody can tell
	DoneWait time.Duration                // how long a turn waits once Done (0: Patience)
	Silence  time.Duration                // the silence check-in, instead of Config.SilenceTimeout (0: that)
}

// Listen starts listening, or stops (nil).
func (ms *ManagedStream) Listen(l *Listening) {
	if ms == nil {
		return
	}
	ms.mu.Lock()
	was := ms.listening != nil
	ms.listening, ms.listeningGen = l, ms.payloadGen
	ms.updateActivity()
	ms.mu.Unlock()
	if on := l != nil; on != was {
		ms.logger.Info("Listening", "on", on)
	}
}

// listeningTo is what the stream listens for, nil when it does not.
func (ms *ManagedStream) listeningTo() *Listening {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.listening
}

// endListeningOnSpeech stops listening once the agent speaks in a generation after the one that started it.
func (ms *ManagedStream) endListeningOnSpeech(gen int) {
	ms.mu.Lock()
	ended := ms.listening != nil && gen > ms.listeningGen
	if ended {
		ms.listening = nil
	}
	ms.mu.Unlock()
	if ended {
		ms.logger.Info("Listening", "on", false, "reason", "the agent spoke")
	}
}

// wait is how long a turn waits for more; whole: its words are the whole of it.
func (l *Listening) wait(whole bool) time.Duration {
	if whole && l.DoneWait > 0 {
		return l.DoneWait
	}
	return l.Patience
}

// done reports whether the words are the whole of it.
func (l *Listening) done(transcript string) bool { return l.Done != nil && l.Done(transcript) }

// holdsSpeculation reports whether a guess at the reply to these words would be wasted: the turn waits for more of what is
// being dictated. The judge, where there is one, is asked here, ahead of the turn.
func (ms *ManagedStream) holdsSpeculation(ctx context.Context, words, lastPiece string) bool {
	if j := ms.activeJudge(); j != nil {
		if v := ms.judgeDictation(j, words, lastPiece).wait(ctx, dictationJudgeBudget); v != "" {
			return v == DictationMore
		}
	}
	l := ms.listeningTo()
	return l != nil && !l.done(words)
}

// keepUnheardPiece keeps, while listening, an utterance the recogniser returned nothing for: it is transcribed together
// with the next one instead of asking the caller to repeat in the middle of what they are dictating. Reports whether it did.
func (ms *ManagedStream) keepUnheardPiece(audio []byte, endedAt time.Time, seq int, noSpeech float64) bool {
	ms.mu.Lock()
	keep := ms.listening != nil && noSpeech <= 0.7 && len(audio) > 0
	if keep {
		ms.carry = &committedUtterance{audio: audio, endedAt: endedAt, gen: ms.payloadGen, seq: seq}
	}
	ms.mu.Unlock()
	if keep {
		ms.logger.Info("Listening: nothing recognised in this piece on its own, kept for the next one",
			"audio_bytes", len(audio), "no_speech_prob", noSpeech)
	}
	return keep
}

// SetSilentTools names the tools whose call ends the agent's turn even when nothing was said with it (listening on): the
// model is not asked for a reply after them, and nothing is said while they run. Words said with the call are the turn.
func (o *Orchestrator) SetSilentTools(names ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.silentTools = map[string]bool{}
	for _, n := range names {
		o.silentTools[n] = true
	}
}

// endsSilently reports whether a round of calls ends the turn with nothing more to say: at least one silent tool, and
// every other call one that ends a turn and has no more to say.
func (o *Orchestrator) endsSilently(calls []toolExchange) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	silent := false
	for _, c := range calls {
		switch {
		case strings.HasPrefix(strings.TrimSpace(c.Result), `{"error"`), resultContinues(c.Result):
			return false
		case o.silentTools[c.TC.Name]:
			silent = true
		case !o.finalTools[c.TC.Name]:
			return false
		}
	}
	return silent
}

func (o *Orchestrator) isSilentTool(name string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.silentTools[name]
}

// IsListening reports whether the stream is listening.
func (ms *ManagedStream) IsListening() bool { return ms != nil && ms.listeningTo() != nil }

// AddQuietTools adds to the tools SetQuietTools named.
func (o *Orchestrator) AddQuietTools(names ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.quietTools == nil {
		o.quietTools = map[string]bool{}
	}
	for _, n := range names {
		o.quietTools[n] = true
	}
}

// SetMoveTools names the tools that move the conversation to another step (a workflow's). What the model writes with one is
// not said when it can be helped: the next step speaks, and a model without thinking often writes there what it is about to
// do ("It's a match, so I move on", in English, on a Spanish call; Claude Haiku 5.5, 2 of 8 simulated calls, 2026-11-25). A
// sentence is therefore handed to the voice when the next piece of the reply arrives, some tens of milliseconds later, and
// dropped when that is a move. Nothing changes for a call with no such tools.
func (o *Orchestrator) SetMoveTools(names ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.moveTools = map[string]bool{}
	for _, n := range names {
		o.moveTools[n] = true
	}
}

func (o *Orchestrator) isMoveTool(name string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.moveTools[name]
}

func (o *Orchestrator) hasMoveTools() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.moveTools) > 0
}
