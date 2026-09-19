package orchestrator

import (
	"context"
	"testing"
)

// newHeldAudioStream builds a stream sitting in a tentative barge-in: playback is
// muted (StateListening) but nothing has been confirmed, so frames arriving now are
// candidates for a resume rather than for the bin.
func newHeldAudioStream(t *testing.T) *ManagedStream {
	t.Helper()
	// A real ctx matters: emitWithGen opens with `select { case <-ms.ctx.Done() }`
	// and swallows every panic in a deferred recover, so a nil ctx makes the whole
	// function a silent no-op and every assertion below passes for the wrong reason.
	ms := &ManagedStream{
		ctx:            context.Background(),
		session:        NewConversationSession("test"),
		state:          StateListening,
		pendingBargeIn: true,
		playbackRate:   44100,
		events:         make(chan OrchestratorEvent, 256),
	}
	ms.ttsCancel = func() {}
	return ms
}

func drainAudio(ch chan OrchestratorEvent) [][]byte {
	var out [][]byte
	for {
		select {
		case ev := <-ch:
			if ev.Type == AudioChunk {
				if b, ok := ev.Data.([]byte); ok {
					out = append(out, b)
				}
			}
		default:
			return out
		}
	}
}

// The bug this whole mechanism exists for: a reply generated entirely inside a
// tentative barge-in window used to be discarded frame by frame, so rejecting the
// barge-in resumed into silence. Measured on production, a 1.88s reply was produced
// and thrown away in full while a spurious VAD start held the gate shut.
func TestTentativeBargeInHoldsAudioAndResumeFlushesIt(t *testing.T) {
	ms := newHeldAudioStream(t)

	frames := [][]byte{{1, 1}, {2, 2}, {3, 3}}
	for _, f := range frames {
		ms.emitWithGen(AudioChunk, f, 0)
	}

	// Nothing reaches the caller while the gate is shut.
	if got := drainAudio(ms.events); len(got) != 0 {
		t.Fatalf("emitted %d frames during a tentative barge-in, want 0", len(got))
	}
	if ms.heldAudioBytes != 6 {
		t.Fatalf("heldAudioBytes = %d, want 6 (frames must be kept, not dropped)", ms.heldAudioBytes)
	}

	// Caller turned out not to be speaking: the barge-in is rejected and playback
	// resumes. The frames held in the meantime must actually arrive.
	ms.vadSpeaking = false
	ms.resolvePendingBargeIn()

	if ms.state != StateSpeaking {
		t.Fatalf("state = %v, want StateSpeaking after a rejected barge-in", ms.state)
	}
	got := drainAudio(ms.events)
	if len(got) != len(frames) {
		t.Fatalf("flushed %d frames, want %d — a resume that delivers nothing is the bug", len(got), len(frames))
	}
	for i := range frames {
		if string(got[i]) != string(frames[i]) {
			t.Errorf("frame %d = %v, want %v (order must be preserved)", i, got[i], frames[i])
		}
	}
	if ms.heldAudioBytes != 0 {
		t.Errorf("heldAudioBytes = %d after flush, want 0", ms.heldAudioBytes)
	}
}

// A confirmed barge-in is a real interruption: the held frames belong to a response
// the caller talked over, and playing them afterwards is exactly what barge-in is
// supposed to prevent.
func TestConfirmedBargeInDiscardsHeldAudio(t *testing.T) {
	ms := newHeldAudioStream(t)
	ms.emitWithGen(AudioChunk, []byte{1, 2, 3, 4}, 0)
	if ms.heldAudioBytes == 0 {
		t.Fatal("precondition: expected audio to be held")
	}

	ms.confirmBargeInIfPending()

	if ms.heldAudioBytes != 0 || len(ms.heldAudio) != 0 {
		t.Errorf("held audio survived a confirmed barge-in (%d bytes) — it would surface on a later turn",
			ms.heldAudioBytes)
	}
}

// Staying muted because the caller is still talking must not clear the pending flag.
// Clearing it made the resume this function documents unreachable: the next call
// would take the !pendingBargeIn branch and force StateIdle, ending the very
// response it was meant to resume, and frames arriving meanwhile would be dropped
// rather than held.
func TestStillSpeakingKeepsBargeInPendingSoResumeStaysPossible(t *testing.T) {
	ms := newHeldAudioStream(t)
	ms.vadSpeaking = true

	ms.resolvePendingBargeIn()
	if ms.state != StateListening {
		t.Fatalf("state = %v, want StateListening while the caller is still speaking", ms.state)
	}
	if !ms.pendingBargeIn {
		t.Fatal("pending barge-in was cleared while still muted — the documented callback can never resume")
	}

	// Frames arriving in this window are still candidates for a resume.
	ms.emitWithGen(AudioChunk, []byte{9, 9}, 0)
	if ms.heldAudioBytes != 2 {
		t.Fatalf("heldAudioBytes = %d, want 2 — frames must still be held while muted", ms.heldAudioBytes)
	}

	// The caller stops; the callback resumes and delivers what was held.
	ms.vadSpeaking = false
	ms.resolvePendingBargeIn()
	if ms.state != StateSpeaking {
		t.Fatalf("state = %v, want StateSpeaking once the caller stopped", ms.state)
	}
	if got := drainAudio(ms.events); len(got) != 1 {
		t.Errorf("flushed %d frames, want 1", len(got))
	}
}

// Holding is bounded. A barge-in long enough to overflow the buffer is almost
// certainly real, and dumping seconds of stale audio at a caller who has moved on
// is worse than not resuming at all.
func TestHeldAudioIsBounded(t *testing.T) {
	ms := newHeldAudioStream(t)
	cap := ms.maxHeldAudioBytesLocked()

	frame := make([]byte, 8820) // 100ms at 44.1kHz mono 16-bit
	for i := 0; i < (cap/len(frame))+50; i++ {
		ms.emitWithGen(AudioChunk, frame, 0)
	}

	if ms.heldAudioBytes > cap {
		t.Errorf("heldAudioBytes = %d, exceeds cap %d — unbounded per-stream growth", ms.heldAudioBytes, cap)
	}
	if ms.heldAudioBytes == 0 {
		t.Error("nothing held at all; the cap should bound the buffer, not disable it")
	}
}

// A stale resolve from a superseded utterance must not flush one generation's audio
// into another's playback.
func TestHeldAudioIsNotFlushedAcrossGenerations(t *testing.T) {
	ms := newHeldAudioStream(t)
	ms.emitWithGen(AudioChunk, []byte{1, 2}, 0)

	// A newer turn has since started.
	ms.payloadGen = 1

	ms.resolvePendingBargeIn() // stale: pendingBargeGen (0) != payloadGen (1)

	if got := drainAudio(ms.events); len(got) != 0 {
		t.Errorf("flushed %d frames from a superseded generation into the current turn", len(got))
	}
}
