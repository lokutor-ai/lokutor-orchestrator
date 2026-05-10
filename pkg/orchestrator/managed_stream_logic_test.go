package orchestrator

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestManagedStream_InterruptionLogic(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	ms.vad = NewRMSVAD(0.1, 100*time.Millisecond)

	ms.mu.Lock()
	ms.isThinking = true
	ms.mu.Unlock()

	ms.internalInterrupt()

	if ms.isThinking {
		t.Error("isThinking should be false after interruption")
	}
	if ms.isSpeaking {
		t.Error("isSpeaking should be false after interruption")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	select {
	case ev := <-ms.events:
		if ev.Type != Interrupted {
			t.Errorf("expected Interrupted event, got %v", ev.Type)
		}
	case <-ctx.Done():
		t.Error("timed out waiting for Interrupted event")
	}
}

func TestManagedStream_EchoGuard(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	vad := NewRMSVAD(0.02, 100*time.Millisecond)
	ms.vad = vad

	if vad.Threshold() != 0.02 {
		t.Errorf("expected threshold 0.02, got %f", vad.Threshold())
	}

	ms.NotifyAudioPlayed()

	chunk := make([]byte, 200)
	for i := 0; i < len(chunk); i += 2 {

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

	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now().Add(-500 * time.Millisecond)
	ms.mu.Unlock()

	err = ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	if vad.IsSpeaking() {

	} else {

	}
}

func TestManagedStream_StaleAudioDiscard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:    make(chan OrchestratorEvent, 10),
		session:   &ConversationSession{ID: "test"},
		ctx:       ctx,
		writeChan: make(chan []byte, 10),
	}
	go ms.processBackgroundAudio()

	ms.isSpeaking = false
	ms.emit(AudioChunk, []byte("stale"))

	select {
	case <-ms.events:
		t.Error("should have discarded audio chunk when not speaking")
	default:

	}

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
func TestManagedStream_EndToEndLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:    make(chan OrchestratorEvent, 10),
		session:   &ConversationSession{ID: "test"},
		ctx:       ctx,
		writeChan: make(chan []byte, 10),
	}
	go ms.processBackgroundAudio()

	base := time.Now()
	start := base
	played := base.Add(250 * time.Millisecond)

	ms.mu.Lock()
	ms.userSpeechEndTime = start
	ms.lastAudioSentAt = played
	ms.mu.Unlock()

	if got := ms.GetEndToEndLatency(); got != int64(250) {
		t.Fatalf("expected 250ms, got %dms", got)
	}
}

func TestManagedStream_LatencyBreakdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:    make(chan OrchestratorEvent, 10),
		session:   &ConversationSession{ID: "test"},
		ctx:       ctx,
		writeChan: make(chan []byte, 10),
	}
	go ms.processBackgroundAudio()

	base := time.Now()
	ms.mu.Lock()
	ms.userSpeechEndTime = base
	ms.sttStartTime = base.Add(10 * time.Millisecond)
	ms.sttEndTime = base.Add(110 * time.Millisecond)
	ms.llmStartTime = base.Add(130 * time.Millisecond)
	ms.llmEndTime = base.Add(380 * time.Millisecond)
	ms.ttsStartTime = base.Add(400 * time.Millisecond)
	ms.ttsFirstChunkTime = base.Add(520 * time.Millisecond)
	ms.ttsEndTime = base.Add(900 * time.Millisecond)
	ms.botSpeakStartTime = base.Add(395 * time.Millisecond)
	ms.lastAudioSentAt = base.Add(525 * time.Millisecond)
	ms.mu.Unlock()

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

func TestManagedStream_ExportLastUserAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:    make(chan OrchestratorEvent, 10),
		session:   &ConversationSession{ID: "test"},
		ctx:       ctx,
		writeChan: make(chan []byte, 10),
	}
	go ms.processBackgroundAudio()

	user := make([]byte, 44100/20*2)
	for i := 0; i < len(user)-1; i += 2 {
		user[i] = 0x40
		user[i+1] = 0x00
	}

	ms.mu.Lock()
	ms.lastUserAudio = make([]byte, len(user))
	copy(ms.lastUserAudio, user)
	ms.mu.Unlock()

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

func TestManagedStream_SetPlaybackRate(t *testing.T) {
	ms := &ManagedStream{}
	ms.SetEchoSampleRates(48000, 16000)
	if ms.playbackRate != 48000 {
		t.Fatalf("rate not set: %d", ms.playbackRate)
	}
}

func TestManagedStream_StreamsToSTT(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:    make(chan OrchestratorEvent, 10),
		session:   &ConversationSession{ID: "test"},
		ctx:       ctx,
		audioBuf:  new(bytes.Buffer),
		writeChan: make(chan []byte, 100),
	}
	go ms.processBackgroundAudio()
	ms.vad = NewRMSVAD(0.02, 50*time.Millisecond)

	ch := make(chan []byte, 4)
	ms.mu.Lock()
	ms.sttChan = ch
	ms.mu.Unlock()

	played := make([]byte, 4410*2)
	for i := 0; i < len(played)-1; i += 2 {
		val := int16(8000)
		played[i] = byte(val)
		played[i+1] = byte(val >> 8)
	}

	err := ms.Write(played)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected audio to be forwarded to STT")
	}

	ms.mu.Lock()
	if len(ms.lastUserAudio) == 0 {
		ms.mu.Unlock()
		t.Fatal("expected lastUserAudio to contain the forwarded data")
	}
	ms.mu.Unlock()
}
