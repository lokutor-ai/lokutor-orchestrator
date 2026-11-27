package orchestrator

import (
	"context"
	"testing"
	"time"
)

// holdsStream is bcStream with Config.BackchannelInHolds on.
func holdsStream(t *testing.T) (*ManagedStream, *bcVAD) {
	t.Helper()
	ms, vad := bcStream(t, &bcPickLLM{})
	cfg := ms.orch.GetConfig()
	cfg.BackchannelInHolds = true
	ms.orch.UpdateConfig(cfg)
	return ms, vad
}

// holdPause is readyPause with speech of its own and the gate's decision taken in the pause.
func holdPause(ms *ManagedStream, vad *bcVAD, pause, speech time.Duration, rule string) time.Time {
	now := readyPause(ms, vad, pause)
	ms.bc.mu.Lock()
	ms.bc.speech = speech
	ms.bc.mu.Unlock()
	if rule != "" {
		ms.noteEOURuleForBackchannel(rule, now.Add(-pause/2))
	}
	return now
}

func TestBackchannelInHoldsPlaysOnlyWhereTheGateHolds(t *testing.T) {
	ms, vad := holdsStream(t)
	// The gate answers at once: the agent is taking the turn, a listener sound is not what a person makes.
	now := holdPause(ms, vad, 270*time.Millisecond, 3*time.Second, "answer_now")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound in a pause the gate answered at once")
	}
	// The gate holds the turn (the caller is mid-thought) after one phrase of speech: the sound plays.
	time.Sleep(2 * time.Millisecond)
	now = holdPause(ms, vad, 270*time.Millisecond, 1100*time.Millisecond, "waited")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 50*time.Millisecond)) == 0 {
		t.Fatal("no sound in a held pause after 1.1 s of speech")
	}
}

func TestBackchannelInHoldsWaitsForTheGatesDecision(t *testing.T) {
	ms, vad := holdsStream(t)
	now := holdPause(ms, vad, 260*time.Millisecond, 2*time.Second, "")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound before the gate decided the pause")
	}
	ms.bc.mu.Lock()
	fired := !ms.bc.firedPause.IsZero()
	ms.bc.mu.Unlock()
	if fired {
		t.Fatal("the pause was used up before the gate decided it")
	}
	// The decision lands (open hold) and the next frame plays it.
	ms.noteEOURuleForBackchannel("open_wait", time.Now())
	ms.backchannelAfterVAD(make([]byte, 640), true, time.Now())
	if len(bcAudio(ms, 50*time.Millisecond)) == 0 {
		t.Fatal("no sound once the gate held the pause")
	}
	// A decision older than the pause belongs to another pause: not this one's.
	ms2, vad2 := holdsStream(t)
	ms2.noteEOURuleForBackchannel("waited", time.Now().Add(-time.Second))
	now = holdPause(ms2, vad2, 700*time.Millisecond, 2*time.Second, "")
	ms2.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms2, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound on an earlier pause's decision")
	}
}

func TestBackchannelInHoldsOverAReplyTheCallerStopped(t *testing.T) {
	ms, vad := holdsStream(t)
	ms.SetTransportPlaysThroughBargeIn(true)
	// The caller cut the agent's reply off: playback was stopped under them and nothing resumes it. They have the floor.
	ms.mu.Lock()
	ms.payloadGen = 3
	ms.pendingBargeIn, ms.pendingBargeGen = true, 3
	ms.onset = heardSnapshot{gen: 3, at: time.Now().Add(-3 * time.Second), heardAtStart: 2 * time.Second, stopped: true}
	ms.mu.Unlock()
	now := holdPause(ms, vad, 270*time.Millisecond, 3*time.Second, "open_wait")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 50*time.Millisecond)) == 0 {
		t.Fatal("no sound for a caller who has the floor over a stopped reply")
	}

	// The agent's reply is still being synthesised and was heard: a false alarm would resume it. No sound.
	ms2, vad2 := holdsStream(t)
	ms2.SetTransportPlaysThroughBargeIn(true)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	ms2.mu.Lock()
	ms2.payloadGen = 2
	ms2.pendingBargeIn, ms2.pendingBargeGen = true, 2
	ms2.ttsCancel = cancel
	ms2.onset = heardSnapshot{gen: 2, at: time.Now().Add(-time.Second), heardAtStart: 800 * time.Millisecond}
	ms2.mu.Unlock()
	now = holdPause(ms2, vad2, 270*time.Millisecond, 3*time.Second, "waited")
	ms2.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms2, 30*time.Millisecond)) != 0 {
		t.Fatal("a sound while the agent's heard reply would resume")
	}
	ms2.mu.Lock()
	ms2.ttsCancel = nil
	ms2.pendingBargeIn = false
	ms2.mu.Unlock()
}

func TestBackchannelWithoutHoldsKeepsTheOldRule(t *testing.T) {
	ms, vad := bcStream(t, &bcPickLLM{})
	// An answer-at-once decision does not matter without the switch, and 1.1 s of speech is still too little.
	now := holdPause(ms, vad, 270*time.Millisecond, 1100*time.Millisecond, "waited")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 30*time.Millisecond)) != 0 {
		t.Fatal("1.1 s of speech played without the switch")
	}
	time.Sleep(2 * time.Millisecond)
	now = holdPause(ms, vad, 270*time.Millisecond, 2500*time.Millisecond, "answer_now")
	ms.backchannelAfterVAD(make([]byte, 640), true, now)
	if len(bcAudio(ms, 50*time.Millisecond)) == 0 {
		t.Fatal("the old rule no longer plays at an answered pause")
	}
}
