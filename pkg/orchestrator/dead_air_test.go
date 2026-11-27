package orchestrator

import (
	"testing"
	"time"
)

func deadAirStream(t *testing.T, ms int) *ManagedStream {
	t.Helper()
	s, _ := bcStream(t, &bcPickLLM{})
	cfg := s.orch.GetConfig()
	cfg.DeadAirGuardMs = ms
	s.orch.UpdateConfig(cfg)
	s.mu.Lock()
	s.state = StateProcessing
	s.utteranceSeq = 4
	s.mu.Unlock()
	return s
}

func TestDeadAirGuardSoundsWhenTheReplyIsLate(t *testing.T) {
	ms := deadAirStream(t, 40)
	before := len(ms.session.GetContextCopy())
	ms.armDeadAirGuard(4)
	a := bcAudio(ms, 150*time.Millisecond)
	if len(a) == 0 {
		t.Fatal("no sound with the reply 40 ms late")
	}
	// No pick: "ah" (ack); the test bank has none, so its neutral clip (300 ms).
	if len(a) != 2*16*300 {
		t.Fatalf("%d bytes, want one 300 ms clip", len(a))
	}
	if got := len(ms.session.GetContextCopy()); got != before {
		t.Fatal("the conversation changed")
	}
	ms.mu.Lock()
	end := ms.backchannelEnd
	ms.mu.Unlock()
	if !end.After(time.Now()) {
		t.Fatal("the reply is not scheduled after the sound")
	}
}

func TestDeadAirGuardQuietWhenTheReplyIsOut(t *testing.T) {
	ms := deadAirStream(t, 40)
	ms.armDeadAirGuard(4)
	ms.mu.Lock()
	ms.ttsFirstChunkTime = time.Now() // the reply's first audio went out after the commit
	ms.mu.Unlock()
	if len(bcAudio(ms, 120*time.Millisecond)) != 0 {
		t.Fatal("a sound after the reply had started")
	}
	// A newer utterance, or the caller talking: nothing.
	ms2 := deadAirStream(t, 30)
	ms2.armDeadAirGuard(3)
	if len(bcAudio(ms2, 100*time.Millisecond)) != 0 {
		t.Fatal("a sound for an utterance that is not the newest")
	}
	ms3 := deadAirStream(t, 30)
	ms3.mu.Lock()
	ms3.vadSpeaking = true
	ms3.mu.Unlock()
	ms3.armDeadAirGuard(4)
	if len(bcAudio(ms3, 100*time.Millisecond)) != 0 {
		t.Fatal("a sound over the caller")
	}
	// Off.
	ms4 := deadAirStream(t, 0)
	ms4.armDeadAirGuard(4)
	if len(bcAudio(ms4, 80*time.Millisecond)) != 0 {
		t.Fatal("a sound with the guard off")
	}
}

func TestDeadAirGuardUsesAnAnsweringPick(t *testing.T) {
	ms := deadAirStream(t, 30)
	ms.bc.mu.Lock()
	ms.bc.pick = BackchannelSympathy
	ms.bc.mu.Unlock()
	ms.armDeadAirGuard(4)
	if a := bcAudio(ms, 150*time.Millisecond); len(a) != 2*16*400 {
		t.Fatalf("%d bytes, want the 400 ms sympathy clip", len(a))
	}
	// A listener sound just played: not another one.
	ms2 := deadAirStream(t, 30)
	ms2.bc.mu.Lock()
	ms2.bc.lastAt = time.Now()
	ms2.bc.mu.Unlock()
	ms2.armDeadAirGuard(4)
	if len(bcAudio(ms2, 100*time.Millisecond)) != 0 {
		t.Fatal("two sounds back to back")
	}
}
