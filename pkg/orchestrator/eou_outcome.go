package orchestrator

import (
	"fmt"
	"sync"
	"time"
)

// End-of-turn decision and outcome logging (2026-11-03). Every end-of-turn decision writes one "eou_decision" line, and the
// next thing that happens writes one "eou_outcome" line for it: did the caller resume within eouResumeWindow of the
// decision, and in which state was the agent. Both carry only derived features (scores, counts, the rule that fired), never
// the words, so the pair can be exported and used to calibrate the end-of-turn model on real outcomes. Field names are
// stable; the session_id and transport come with the logger.

const eouResumeWindow = 2 * time.Second

type eouPending struct {
	seq    int
	at     time.Time
	logged bool
}

type eouTracker struct {
	mu      sync.Mutex
	pending *eouPending
	events  int // resumes seen in this call (adaptive patience)
	calm    int // decisions in a row that nobody resumed after: three of them take one event back
}

// logEOUDecision records the decision just taken for utterance seq. rule is "answer_now", "waited" or "open_wait";
// waitMs is the wait the gate applied (0 for answer_now).
// eouAdaptMs is the extra wait this caller has earned in this call: EndOfTurnAdaptStepMs per time they resumed, capped. No words involved: only what the caller
// has shown about how they speak.
func (ms *ManagedStream) eouAdaptMs() int {
	step := ms.orch.config.EndOfTurnAdaptStepMs
	if step <= 0 {
		return 0
	}
	ms.eou.mu.Lock()
	n := ms.eou.events
	ms.eou.mu.Unlock()
	a := step * n
	if max := ms.orch.config.EndOfTurnAdaptMaxMs; max > 0 && a > max {
		a = max
	}
	return a
}

func (ms *ManagedStream) logEOUDecision(seq int, result TranscriptionResult, transcript, rule string, waitMs int, lexicalComplete bool, turno [4]float32, turnoFrames int, adaptMs int) {
	// The previous decision, if still unresolved, ended without the caller resuming before this one.
	ms.eou.mu.Lock()
	prev := ms.eou.pending
	ms.eou.pending = &eouPending{seq: seq, at: time.Now()}
	ms.eou.mu.Unlock()
	if prev != nil {
		ms.logger.Info("eou_outcome", "turn_seq", prev.seq, "resumed", false, "resume_ms", nil, "bot_state", "", "reason", "next_decision")
	}
	// Turno's prosody read of the same turn (null when it saw no speech frames): the signal that, with the recogniser's heads, can decide.
	var tc, ti, tb, tw interface{}
	if turnoFrames > 0 {
		tc, ti, tb, tw = turno[0], turno[1], turno[2], turno[3]
	}
	ms.logger.Info("eou_decision",
		"turn_seq", seq,
		"turno_complete", tc, "turno_incomplete", ti, "turno_backchannel", tb, "turno_wait", tw,
		"eou", eouOrNil(result), "eou_alt", eouAltOrNil(result), "eou_text", eouTextOrNil(result),
		"words", countWords(transcript), "chars", len(transcript),
		"rule", rule, "wait_ms", waitMs, "adapt_ms", adaptMs, "lexical_complete", lexicalComplete,
		"threshold", ms.orch.config.EndOfTurnThreshold, "uses_alt", ms.orch.config.EndOfTurnUseAlt)
}

// resolveEOUOutcome is called when a new utterance of the caller starts being processed: if it began within
// eouResumeWindow of the last decision, the caller resumed (resume_ms can be negative: the caller was already speaking
// again while the decision was being taken); otherwise the caller did not.
func (ms *ManagedStream) resolveEOUOutcome(newSpeechStart time.Time) {
	ms.eou.mu.Lock()
	p := ms.eou.pending
	ms.eou.pending = nil
	ms.eou.mu.Unlock()
	if p == nil {
		return
	}
	d := newSpeechStart.Sub(p.at)
	ms.mu.Lock()
	state := fmt.Sprint(ms.state)
	first := ms.ttsFirstChunkTime
	ms.mu.Unlock()
	// The agent's first audio after this decision, if any: what the caller hears as being talked over is speaking while it plays.
	var audioStarted interface{}
	resumedAfterAudio := false
	if !first.IsZero() && first.After(p.at) {
		audioStarted = first.Sub(p.at).Milliseconds()
		resumedAfterAudio = newSpeechStart.After(first)
	}
	if d <= eouResumeWindow {
		ms.eou.mu.Lock()
		ms.eou.events++
		ms.eou.calm = 0
		ms.eou.mu.Unlock()
		ms.logger.Info("eou_outcome", "turn_seq", p.seq, "resumed", true, "resume_ms", d.Milliseconds(), "bot_state", state, "reason", "new_utterance",
			"audio_started_ms", audioStarted, "resumed_after_audio", resumedAfterAudio)
		return
	}
	ms.eou.mu.Lock()
	if ms.eou.calm++; ms.eou.calm >= 3 && ms.eou.events > 0 {
		ms.eou.events--
		ms.eou.calm = 0
	}
	ms.eou.mu.Unlock()
	ms.logger.Info("eou_outcome", "turn_seq", p.seq, "resumed", false, "resume_ms", d.Milliseconds(), "bot_state", state, "reason", "new_utterance_later",
		"audio_started_ms", audioStarted, "resumed_after_audio", resumedAfterAudio)
}

// flushEOUOutcome closes the session's last decision: nothing followed it.
func (ms *ManagedStream) flushEOUOutcome() {
	ms.eou.mu.Lock()
	p := ms.eou.pending
	ms.eou.pending = nil
	ms.eou.mu.Unlock()
	if p != nil {
		ms.logger.Info("eou_outcome", "turn_seq", p.seq, "resumed", false, "resume_ms", nil, "bot_state", "", "reason", "session_end")
	}
}
