package orchestrator

import (
	"context"
	"testing"
	"time"
)

// A full event channel must never swallow audio in silence.
//
// The send was `select { case ms.events <- event: default: }` with an empty default, so once the
// channel filled every subsequent event vanished without a log line. For an AudioChunk that is the
// caller losing part of a reply — or all of it, if the first chunk is the one discarded — and the
// only evidence was the caller's silence. Production showed exactly that shape: TTS synthesised the
// reply, `turn latency` recorded a healthy turn, "Dropping bot audio" was zero, and the caller got
// nothing.
func TestAudioChunkWaitsForRoomRatherThanVanishing(t *testing.T) {
	ms := &ManagedStream{
		ctx:    context.Background(),
		state:  StateSpeaking,
		events: make(chan OrchestratorEvent, 1),
	}
	// Fill it.
	ms.events <- OrchestratorEvent{Type: BotThinking}

	// A consumer that frees a slot shortly after — the transient stall an audio chunk should ride
	// out rather than be dropped into.
	go func() {
		time.Sleep(40 * time.Millisecond)
		<-ms.events
	}()

	done := make(chan struct{})
	go func() {
		ms.emitWithGen(AudioChunk, []byte{1, 2, 3}, 1)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emitWithGen never returned")
	}

	// The chunk must be in the channel, not discarded.
	select {
	case ev := <-ms.events:
		if ev.Type != AudioChunk {
			t.Fatalf("got %v, want the AudioChunk that waited for room", ev.Type)
		}
	default:
		t.Fatal("audio chunk was dropped despite a slot opening 40ms later — " +
			"a dropped chunk is silence the caller hears, and it used to happen with no log at all")
	}
}

// Non-audio events keep the old cheap behaviour: they are not worth blocking the pipeline for, and
// losing a status is recoverable in a way losing audio is not.
func TestNonAudioEventDoesNotBlock(t *testing.T) {
	ms := &ManagedStream{
		ctx:    context.Background(),
		state:  StateSpeaking,
		events: make(chan OrchestratorEvent, 1),
	}
	ms.events <- OrchestratorEvent{Type: BotThinking}

	start := time.Now()
	ms.emitWithGen(BotResponse, "hello", 1)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("a non-audio event blocked for %v; only audio should wait for room", elapsed)
	}
}
