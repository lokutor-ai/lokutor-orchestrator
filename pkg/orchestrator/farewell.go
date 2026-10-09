package orchestrator

import "time"

// Farewell ends the call from the agent's side, gracefully: once the agent is between turns, the model is
// told why the call must end (note) and says a short goodbye in the conversation's language. It is offered
// no tools, so the goodbye cannot turn into an action, or into a tool call written out as text and
// dropped (which is what happened when it was offered end_call on a session that had none). The host
// hangs up itself: the returned channel closes when the goodbye has been generated and handed to the
// voice, and the host waits for it to be heard before ending the call. The host uses it when the account
// behind the call can no longer pay for it.
//
// It waits up to wait for the stream to be between turns (not thinking, not speaking) so it never cuts the
// agent off; past that it goes ahead anyway. It returns nil if the stream is already closed. The host must
// still hang up after a grace period whatever happens: a goodbye that never comes must not keep the line
// open.
func (ms *ManagedStream) Farewell(note string, wait time.Duration) <-chan struct{} {
	if ms == nil || ms.ctx.Err() != nil {
		return nil
	}
	ms.mu.Lock()
	ms.farewellNote = note
	ms.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			if ms.ctx.Err() != nil {
				return
			}
			ms.mu.Lock()
			between := ms.state == StateIdle || ms.state == StateListening || ms.state == StateInterrupted
			ms.mu.Unlock()
			if between {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if ms.ctx.Err() != nil {
			return
		}
		ms.logger.Info("Host is ending the call: agent saying goodbye", "note", note)
		ms.runLLMAndTTS(ms.ctx, farewellTrigger)
	}()
	return done
}
