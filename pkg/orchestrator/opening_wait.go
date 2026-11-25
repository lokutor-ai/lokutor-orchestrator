package orchestrator

import "time"

// Whoever picks up a call speaks first ("¿Dígame?", "Panadería Rosa, buenos días"), and an agent that placed the call and
// opens at once talks over it. With Config.OpeningWait the agent listens first: the other person's words are the first
// turn, answered as any turn, and only when they say nothing does it say its opening. Noise the recogniser makes nothing
// of does not count as having spoken.

// waitForFirstWords waits up to wait for the other person; it reports whether they spoke.
func (ms *ManagedStream) waitForFirstWords(wait time.Duration) (spoke bool) {
	deadline := time.Now().Add(wait)
	for {
		if ms.ctx.Err() != nil {
			return true
		}
		ms.mu.Lock()
		busy := ms.vadSpeaking || ms.inflightUtterances > 0
		ms.mu.Unlock()
		if ms.callerHasTurn() {
			return true
		}
		now := time.Now()
		if busy {
			if d := now.Add(700 * time.Millisecond); d.After(deadline) {
				deadline = d // somebody is speaking, or what they said is being read
			}
		}
		if now.After(deadline) {
			return false
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// openingPreempted reports whether the other person has the floor, for an opening that waited for them.
func (ms *ManagedStream) openingPreempted() bool {
	ms.mu.Lock()
	busy := ms.vadSpeaking || ms.inflightUtterances > 0
	ms.mu.Unlock()
	return busy || ms.callerHasTurn()
}

// callerHasTurn reports whether the conversation has a turn of the caller's, or one being answered.
func (ms *ManagedStream) callerHasTurn() bool {
	ms.mu.Lock()
	answering := ms.state == StateSpeaking
	ms.mu.Unlock()
	if answering {
		return true
	}
	for _, m := range ms.session.GetContextCopy() {
		if m.Role == "user" {
			return true
		}
	}
	return false
}
