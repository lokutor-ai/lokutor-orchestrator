package orchestrator

import "time"

// hostNoteTrigger is the pseudo-transcript HostNote hands runLLMAndTTS: the host learned something
// outside the conversation (a transfer to a person that nobody answered) and the agent speaks about it
// now. Like the other triggers it is never a caller's words and never reaches the history.
const hostNoteTrigger = "[HOST_NOTE]"

// Hold pauses the agent while the host has the caller on hold, for a transfer that is ringing: the
// caller's audio is dropped before the VAD, so nothing they say starts a turn, and the silence check-in
// does not fire. Hold(false) resumes, with the idle clock restarted so the check-in is not due the
// moment the hold ends. Audio dropped while held is counted and logged when the hold ends (CLAUDE.md 6:
// a caller's words must not disappear without a trace).
func (ms *ManagedStream) Hold(on bool) {
	if ms == nil {
		return
	}
	was := ms.held.Swap(on)
	ms.mu.Lock()
	ms.updateActivity()
	ms.mu.Unlock()
	if was && !on {
		if n := ms.heldDropped.Swap(0); n > 0 {
			ms.logger.Info("Hold ended: caller audio dropped while on hold", "chunks", n)
		}
	}
}

// IsHeld reports whether the agent is on hold.
func (ms *ManagedStream) IsHeld() bool { return ms != nil && ms.held.Load() }

// HostNote has the agent speak, with its tools, about something the host learned outside the
// conversation. note is kept in the conversation as a system message, so later turns know it ("the
// transfer to Reception was not answered"); instruction goes on this one request as a user-role note
// saying what to do now, for the reason silenceCheckInInstruction gives (a system message would be
// lifted into the system prompt and leave the conversation ending on the agent's own reply). Either may
// be empty. It waits up to wait for the stream to be between turns so it never cuts the agent off, then
// goes ahead. The returned channel closes once the response has been generated and handed to the voice;
// it is nil if the stream is already closed.
func (ms *ManagedStream) HostNote(note, instruction string, wait time.Duration) <-chan struct{} {
	if ms == nil || ms.ctx.Err() != nil {
		return nil
	}
	if note != "" && ms.session != nil {
		ms.session.AddMessage("system", note)
	}
	ms.mu.Lock()
	ms.hostNoteInstruction = instruction
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
		ms.logger.Info("Host note: agent speaking")
		ms.runLLMAndTTS(ms.ctx, hostNoteTrigger)
	}()
	return done
}
