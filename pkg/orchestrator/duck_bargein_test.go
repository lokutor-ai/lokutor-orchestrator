package orchestrator

import (
	"context"
	"testing"
	"time"
)

func duckStream(t *testing.T) *ManagedStream {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{responses: []string{"Uno dos tres cuatro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("duck"))
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	stream.SetTransportDucksOnBargeIn(true)
	stream.lastUtt = &committedUtterance{}
	stream.runLLMAndTTS(context.Background(), "cuenta") // synthesis done; one second still to play
	time.Sleep(300 * time.Millisecond)
	drainUntil(stream, 50*time.Millisecond)
	return stream
}

// A caller's "sí" over a phone reply: the transport ducks, nothing is held or cut, the false alarm puts it back at
// full level, and the reply's timeline was never shifted (it kept playing).
func TestDuck_BackchannelNeverHoldsOrCutsTheReply(t *testing.T) {
	stream := duckStream(t)
	stream.mu.Lock()
	endBefore := stream.playout.end
	stream.mu.Unlock()
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	pending := stream.pendingBargeIn && stream.pendingBargeGen == stream.payloadGen
	end := stream.playout.end
	stream.vadSpeaking = false
	stream.mu.Unlock()
	if !pending {
		t.Fatal("speech over playout must be a tentative barge-in on a ducking transport")
	}
	if !end.Equal(endBefore) {
		t.Fatal("the onset cut or moved the playout: the transport keeps playing")
	}
	stream.stopPlayThroughIfDue(time.Now()) // not due
	stream.resolvePendingBargeIn()
	seen := drainUntil(stream, 300*time.Millisecond)
	for _, bad := range []EventType{HoldPlayback, StopPlayback, Interrupted, BotResponseTruncated} {
		if _, ok := seen[bad]; ok {
			t.Fatalf("a false alarm emitted %v", bad)
		}
	}
	if ev, ok := seen[BotResumed]; !ok || ev.Data != "speaking" {
		t.Fatalf("BotResumed = %+v (seen %v)", ev, ok)
	}
	stream.mu.Lock()
	moved := !stream.playout.end.Equal(endBefore)
	stream.mu.Unlock()
	if moved {
		t.Fatal("a ducked reply's timeline was shifted on resume, as if it had paused")
	}
}

// A caller who keeps talking: the transport is told to HOLD (not discard), what was heard is what played until then,
// and a false alarm after that plays it on from the hold, everything after it that much later.
func TestDuck_SustainedSpeechHoldsAndResumesFromTheHold(t *testing.T) {
	t.Setenv("BARGE_IN_PLAYTHROUGH_MS", "150")
	stream := duckStream(t)
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	onsetHeard := stream.onset.dur
	endBefore := stream.playout.end
	stream.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	stream.stopPlayThroughIfDue(time.Now())
	seen := drainUntil(stream, 100*time.Millisecond)
	if _, ok := seen[HoldPlayback]; !ok {
		t.Fatalf("no HoldPlayback (seen %v)", seen)
	}
	for _, bad := range []EventType{StopPlayback, BotResponseTruncated} {
		if _, ok := seen[bad]; ok {
			t.Fatalf("a hold emitted %v", bad)
		}
	}
	stream.mu.Lock()
	held, heard, heldAt := stream.onset.stopped, stream.onset.dur, stream.onset.heldAt
	notCut := stream.playout.end.Equal(endBefore)
	stream.mu.Unlock()
	if !held || !notCut {
		t.Fatalf("held=%v timeline kept=%v", held, notCut)
	}
	if heard < onsetHeard+150*time.Millisecond {
		t.Fatalf("heard %v at the hold, %v at the onset: it played on (ducked) in between", heard, onsetHeard)
	}
	stream.stopPlayThroughIfDue(time.Now())
	if _, ok := drainUntil(stream, 50*time.Millisecond)[HoldPlayback]; ok {
		t.Fatal("HoldPlayback emitted twice")
	}
	time.Sleep(100 * time.Millisecond)
	stream.mu.Lock()
	stream.vadSpeaking = false
	stream.mu.Unlock()
	stream.resolvePendingBargeIn()
	if ev := waitForEventType(t, stream, BotResumed, time.Second); ev.Data != "speaking" {
		t.Fatalf("BotResumed = %v", ev.Data)
	}
	stream.mu.Lock()
	shift := stream.playout.end.Sub(endBefore)
	stream.mu.Unlock()
	if want := time.Since(heldAt); shift < want-50*time.Millisecond || shift > want+50*time.Millisecond {
		t.Fatalf("the rest was shifted %v, held for ~%v", shift, want)
	}
}

// Confirmed while ducked: what was heard runs to the confirmation (it kept playing), and it is cut there.
func TestDuck_ConfirmedWhileDuckedHeardUntilTheConfirmation(t *testing.T) {
	stream := duckStream(t)
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	onsetHeard := stream.onset.dur
	stream.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	stream.confirmBargeInIfPending()
	stream.mu.Lock()
	heard := stream.onset.dur
	stream.mu.Unlock()
	if heard < onsetHeard+100*time.Millisecond {
		t.Fatalf("heard %v at the confirmation, %v at the onset", heard, onsetHeard)
	}
	seen := drainUntil(stream, 200*time.Millisecond)
	if _, ok := seen[Interrupted]; !ok {
		t.Fatalf("no Interrupted (seen %v)", seen)
	}
	tr, ok := seen[BotResponseTruncated].Data.(TruncatedResponse)
	if !ok || tr.HeardMs < heard.Milliseconds()-20 {
		t.Fatalf("truncated = %+v, heard %v", seen[BotResponseTruncated], heard)
	}
}

// Without the switch nothing changes: a pausing transport still shifts on resume and never gets HoldPlayback.
func TestDuck_OffLeavesPausingTransportAlone(t *testing.T) {
	t.Setenv("BARGE_IN_PLAYTHROUGH_MS", "50")
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{responses: []string{"Uno dos tres cuatro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("pause"))
	defer stream.Close()
	stream.SetPlaybackRate(16000)
	stream.SetTransportPausesOnBargeIn(true)
	stream.runLLMAndTTS(context.Background(), "cuenta")
	time.Sleep(300 * time.Millisecond)
	drainUntil(stream, 50*time.Millisecond)
	stream.onVADStart(StateIdle)
	time.Sleep(100 * time.Millisecond)
	stream.stopPlayThroughIfDue(time.Now())
	seen := drainUntil(stream, 100*time.Millisecond)
	if _, ok := seen[HoldPlayback]; ok {
		t.Fatal("HoldPlayback on a pausing transport")
	}
	if _, ok := seen[StopPlayback]; ok {
		t.Fatal("StopPlayback on a pausing transport")
	}
}
