package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_Interruption(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("test")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Simulate user speaking
	// We need loud enough samples to trigger RMS VAD
	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		loudChunk[i] = 0xFF
		loudChunk[i+1] = 0x7F
	}

	// Send multiple chunks to satisfy the VAD confirmation requirement (3 chunks)
	stream.Write(loudChunk)
	stream.Write(loudChunk)
	err := stream.Write(loudChunk)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Check for USER_SPEAKING event
	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING, got %v", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING")
	}
}

func TestManagedStream_EchoSuppression(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	// Base threshold 0.1
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("test")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// 1. Manually set lastAudioSentAt to "just now"
	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now()
	stream.mu.Unlock()

	// 2. Send "Loud" audio (RMS ~0.3) that would normally trigger 0.1 threshold
	// but should be ignored by the 0.35 Echo-Guard threshold.
	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		// Value that generates ~0.25 RMS
		val := int16(32768.0 * 0.25)
		loudChunk[i] = byte(val & 0xFF)
		loudChunk[i+1] = byte(val >> 8)
	}

	// Send 5 chunks (satisfies the VAD confirmation)
	for i := 0; i < 5; i++ {
		stream.Write(loudChunk)
	}

	// 3. Verify NO UserSpeaking event was emitted
	select {
	case ev := <-stream.Events():
		if ev.Type == UserSpeaking {
			t.Errorf("Echo Guard FAILED: Detected UserSpeaking for audio below 0.35 threshold")
		}
	case <-time.After(100 * time.Millisecond):
		// Success - no event emitted
	}

	// 4. Reset lastAudioSentAt to 2 seconds ago (danger zone passed)
	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now().Add(-2 * time.Second)
	stream.mu.Unlock()

	// 5. Send same audio again - it SHOULD trigger UserSpeaking now (threshold is 0.1)
	for i := 0; i < 5; i++ {
		stream.Write(loudChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING after danger zone, got %v", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING after danger zone")
	}
}
