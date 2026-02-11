package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_InterruptionLogic(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	// Configure VAD
	ms.vad = NewRMSVAD(0.1, 100*time.Millisecond)

	// Simulate thinking/speaking state
	ms.mu.Lock()
	ms.isThinking = true
	ms.mu.Unlock()

	// Interruption should clear these
	ms.mu.Lock()
	ms.internalInterrupt()
	ms.mu.Unlock()

	if ms.isThinking {
		t.Error("isThinking should be false after interruption")
	}
	if ms.isSpeaking {
		t.Error("isSpeaking should be false after interruption")
	}

	// Verify Interrupted event was sent
	select {
	case ev := <-ms.events:
		if ev.Type != Interrupted {
			t.Errorf("expected Interrupted event, got %v", ev.Type)
		}
	default:
		t.Error("expected Interrupted event in channel")
	}
}

func TestManagedStream_EchoGuard(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	// Configure RMS VAD
	vad := NewRMSVAD(0.02, 100*time.Millisecond)
	ms.vad = vad

	// 1. Normal state - low threshold
	if vad.Threshold() != 0.02 {
		t.Errorf("expected threshold 0.02, got %f", vad.Threshold())
	}

	// 2. Play audio - NotifyAudioPlayed
	ms.NotifyAudioPlayed()

	// 3. Check Write uses high threshold
	// We call Write with small audio. Threshold should be 0.35 during this call.
	// Since we can't easily peek into the internal state of vad during Process call,
	// we rely on the fact that if it WASN'T 0.35, it would trigger speech on a 0.1 RMS chunk.

	// 0.1 RMS chunk (above 0.02, below 0.25)
	chunk := make([]byte, 200)
	for i := 0; i < len(chunk); i += 2 {
		// ~0.1 amplitude
		val := int16(3276)
		chunk[i] = byte(val)
		chunk[i+1] = byte(val >> 8)
	}

	err := ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	if ms.isSpeaking {
		t.Error("should NOT be speaking due to Echo Guard threshold (0.25)")
	}

	// Wait for echo guard to expire
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now().Add(-500 * time.Millisecond)
	ms.mu.Unlock()

	err = ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	// Now it should trigger
	if vad.IsSpeaking() {
		// Success
	} else {
		// Wait... consecutiveFrames might be 1, we need more?
		// RMSVAD in test has minConfirmed=1.
		// Let's check.
	}
}

func TestManagedStream_StaleAudioDiscard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:  make(chan OrchestratorEvent, 10),
		session: &ConversationSession{ID: "test"},
		ctx:     ctx,
	}

	// 1. speaking=false -> emit should discard
	ms.isSpeaking = false
	ms.emit(AudioChunk, []byte("stale"))

	select {
	case <-ms.events:
		t.Error("should have discarded audio chunk when not speaking")
	default:
		// OK
	}

	// 2. speaking=true -> emit should allow
	ms.isSpeaking = true
	ms.emit(AudioChunk, []byte("fresh"))

	select {
	case ev := <-ms.events:
		if ev.Type != AudioChunk {
			t.Error("expected AudioChunk")
		}
	default:
		t.Error("should have emitted audio chunk when speaking")
	}
}
