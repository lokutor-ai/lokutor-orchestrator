package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_LatencyBreakdown(t *testing.T) {
	ms := &ManagedStream{
		events:        make(chan OrchestratorEvent, 10),
		session:       &ConversationSession{ID: "test"},
		ctx:           context.Background(),
		cmdChan:       make(chan []byte, 10),
		interruptChan: make(chan struct{}, 1),
	}

	base := time.Now()
	ms.userSpeechEnd = base
	ms.sttStartTime = base.Add(10 * time.Millisecond)
	ms.sttEndTime = base.Add(110 * time.Millisecond)
	ms.llmStartTime = base.Add(130 * time.Millisecond)
	ms.llmEndTime = base.Add(380 * time.Millisecond)
	ms.ttsStartTime = base.Add(400 * time.Millisecond)
	ms.ttsFirstChunkTime = base.Add(520 * time.Millisecond)
	ms.ttsEndTime = base.Add(900 * time.Millisecond)
	ms.botSpeakStart = base.Add(395 * time.Millisecond)
	ms.lastAudioSentAt = base.Add(525 * time.Millisecond)

	bd := ms.GetLatencyBreakdown()

	if bd.UserToSTT != int64(110) {
		t.Fatalf("expected UserToSTT 110ms, got %d", bd.UserToSTT)
	}
	if bd.STT != int64(100) {
		t.Fatalf("expected STT 100ms, got %d", bd.STT)
	}
	if bd.UserToLLM != int64(380) {
		t.Fatalf("expected UserToLLM 380ms, got %d", bd.UserToLLM)
	}
	if bd.LLM != int64(250) {
		t.Fatalf("expected LLM 250ms, got %d", bd.LLM)
	}
	if bd.UserToTTSFirstByte != int64(520) {
		t.Fatalf("expected UserToTTSFirstByte 520ms, got %d", bd.UserToTTSFirstByte)
	}
	if bd.LLMToTTSFirstByte != int64(140) {
		t.Fatalf("expected LLMToTTSFirstByte 140ms, got %d", bd.LLMToTTSFirstByte)
	}
	if bd.TTSTotal != int64(500) {
		t.Fatalf("expected TTSTotal 500ms, got %d", bd.TTSTotal)
	}
	if bd.BotStartLatency != int64(395) {
		t.Fatalf("expected BotStartLatency 395ms, got %d", bd.BotStartLatency)
	}
	if bd.UserToPlay != int64(525) {
		t.Fatalf("expected UserToPlay 525ms, got %d", bd.UserToPlay)
	}
}

func TestManagedStream_EndToEndLatency(t *testing.T) {
	ms := &ManagedStream{
		events:        make(chan OrchestratorEvent, 10),
		session:       &ConversationSession{ID: "test"},
		ctx:           context.Background(),
		cmdChan:       make(chan []byte, 10),
		interruptChan: make(chan struct{}, 1),
	}

	base := time.Now()
	ms.userSpeechEnd = base
	ms.lastAudioSentAt = base.Add(250 * time.Millisecond)

	if got := ms.GetEndToEndLatency(); got != int64(250) {
		t.Fatalf("expected 250ms, got %dms", got)
	}
}

func TestManagedStream_ExportLastUserAudio(t *testing.T) {
	ms := &ManagedStream{
		events:        make(chan OrchestratorEvent, 10),
		session:       &ConversationSession{ID: "test"},
		ctx:           context.Background(),
		cmdChan:       make(chan []byte, 10),
		interruptChan: make(chan struct{}, 1),
	}

	user := make([]byte, 44100/20*2)
	for i := 0; i < len(user)-1; i += 2 {
		user[i] = 0x40
		user[i+1] = 0x00
	}

	ms.userAudio = make([]byte, len(user))
	copy(ms.userAudio, user)

	raw, processed := ms.ExportLastUserAudio()
	if raw == nil || processed == nil {
		t.Fatal("expected non-nil raw and processed")
	}
	if len(raw) != len(user) {
		t.Fatalf("raw len mismatch: %d vs %d", len(raw), len(user))
	}
	if len(processed) != len(user) {
		t.Fatalf("processed len mismatch: %d vs %d", len(processed), len(user))
	}
}

func TestManagedStream_InterruptionLogic(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)
	defer ms.Close()

	ms.vad = NewRMSVAD(0.1, 100*time.Millisecond)

	// Simulate a running pipeline: set pipelineCancel, then interrupt
	ctx, cancel := context.WithCancel(context.Background())
	ms.mu.Lock()
	ms.pipelineCancel = cancel
	ms.state = StateProcessing
	ms.mu.Unlock()

	ms.Interrupt()

	select {
	case ev := <-ms.events:
		if ev.Type != Interrupted {
			t.Errorf("expected Interrupted event, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timed out waiting for Interrupted event")
	}

	ms.mu.Lock()
	if ms.state != StateInterrupted {
		t.Errorf("expected state Interrupted after interruption logic, got %v", ms.state)
	}
	if ctx.Err() == nil {
		t.Error("pipeline context should be cancelled after interruption")
	}
	ms.mu.Unlock()
}

func TestManagedStream_StaleAudioDiscard(t *testing.T) {
	ms := &ManagedStream{
		events:        make(chan OrchestratorEvent, 10),
		session:       &ConversationSession{ID: "test"},
		ctx:           context.Background(),
		cmdChan:       make(chan []byte, 10),
		interruptChan: make(chan struct{}, 1),
	}

	ms.emit(AudioChunk, []byte("stale"))
	select {
	case <-ms.events:
		t.Error("should have discarded audio chunk when not speaking")
	default:
	}

	ms.mu.Lock()
	ms.state = StateSpeaking
	ms.mu.Unlock()

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

func TestManagedStream_StreamsToSTT(t *testing.T) {
	t.Skip("Streaming STT path is now handled via state machine; needs integration test")
}
