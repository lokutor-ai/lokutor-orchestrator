package orchestrator

import (
	"context"
	"fmt"
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

	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		loudChunk[i] = 0xFF
		loudChunk[i+1] = 0x7F
	}

	for i := 0; i < 20; i++ {
		stream.Write(loudChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING")
	}
}

func TestManagedStream_EchoSuppression(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}

	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("test")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now()
	stream.mu.Unlock()

	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {

		val := int16(655)
		loudChunk[i] = byte(val & 0xFF)
		loudChunk[i+1] = byte(val >> 8)
	}

	for i := 0; i < 100; i++ {
		stream.Write(loudChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type == UserSpeaking {
			t.Errorf("Echo Guard FAILED: Detected UserSpeaking for audio below echo threshold")
		}
	case <-time.After(100 * time.Millisecond):

	}

	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now().Add(-5 * time.Second)
	stream.mu.Unlock()

	normalChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		val := int16(8192)
		normalChunk[i] = byte(val & 0xFF)
		normalChunk[i+1] = byte(val >> 8)
	}

	for i := 0; i < 100; i++ {
		stream.Write(normalChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking && ev.Type != Interrupted {
			t.Errorf("Expected USER_SPEAKING or INTERRUPTED after danger zone, got %v", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING after danger zone")
	}
}

type MockStreamingSTT struct {
	steps []struct {
		text    string
		isFinal bool
		delay   time.Duration
	}
}

func (m *MockStreamingSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	return TranscriptionResult{}, nil
}
func (m *MockStreamingSTT) Name() string { return "MockStreamingSTT" }
func (m *MockStreamingSTT) StreamTranscribe(ctx context.Context, lang Language, onTranscript func(transcript string, isFinal bool) error) (chan<- []byte, error) {
	ch := make(chan []byte, 8)
	go func() {
		for _, s := range m.steps {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.delay):
			}
			_ = onTranscript(s.text, s.isFinal)
		}
	}()
	return ch, nil
}

func TestManagedStream_MinWordsInterruption(t *testing.T) {
	stt := &MockStreamingSTT{steps: []struct {
		text    string
		isFinal bool
		delay   time.Duration
	}{
		{text: "uh", isFinal: false, delay: 150 * time.Millisecond},
		{text: "i want coffee", isFinal: true, delay: 200 * time.Millisecond},
	}}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1}}

	cfg := DefaultConfig()
	cfg.MinWordsToInterrupt = 3
	vad := NewRMSVAD(0.1, 50*time.Millisecond)
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("u1")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	stream.mu.Lock()
	stream.isSpeaking = true
	stream.mu.Unlock()

	stream.startStreamingSTT(stt)

	select {
	case ev := <-stream.Events():
		if ev.Type == Interrupted {
			t.Fatalf("interrupted too early on partial")
		}
		if ev.Type != TranscriptPartial {
			t.Errorf("expected TranscriptPartial or nothing, got %v", ev.Type)
		}
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case ev := <-stream.Events():
		if ev.Type != Interrupted {
			t.Fatalf("expected Interrupted, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for Interrupted event")
	}
}

type MockLongRunningTTS struct {
	abortCalled bool
	abortCh     chan struct{}
}

func (m *MockLongRunningTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return nil, nil
}
func (m *MockLongRunningTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.abortCh:
			return fmt.Errorf("aborted")
		case <-ticker.C:
			if err := onChunk([]byte{0x01, 0x02}); err != nil {
				return err
			}
		}
	}
}
func (m *MockLongRunningTTS) Abort() error {
	m.abortCalled = true
	select {
	case <-m.abortCh:
	default:
		close(m.abortCh)
	}
	return nil
}
func (m *MockLongRunningTTS) Name() string { return "MockLongTTS" }

func TestManagedStream_TTSAbortOnInterruption(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "user"}
	llm := &MockLLMProvider{completeResult: "assistant reply here"}
	tts := &MockLongRunningTTS{abortCh: make(chan struct{})}
	cfg := DefaultConfig()
	vad := NewRMSVAD(0.02, 100*time.Millisecond)
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("s1")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	go stream.runLLMAndTTS(context.Background(), "hello")

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-stream.Events():
			if ev.Type == BotSpeaking {
				goto started
			}
		case <-deadline:
			t.Fatal("timed out waiting for BotSpeaking")
		}
	}
started:

	stream.interrupt()

	select {
	case ev := <-stream.Events():
		if ev.Type != Interrupted {
			t.Fatalf("expected Interrupted event, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for Interrupted event")
	}

	if !tts.abortCalled {
		t.Fatal("expected TTS Abort() to be called on interruption")
	}
}

func TestManagedStream_InterruptDuringPendingResponse(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	vad := NewRMSVAD(0.02, 50*time.Millisecond)
	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("u2")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	called := false
	stream.mu.Lock()
	stream.responseCancel = func() { called = true }
	stream.mu.Unlock()

	stream.Interrupt()

	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-stream.Events():
			if ev.Type == Interrupted {
				goto interrupted
			}
		case <-timeout:
			t.Fatal("timed out waiting for Interrupted event")
		}
	}
interrupted:

	if !called {
		t.Fatal("expected responseCancel to be invoked by internalInterrupt")
	}
}

func TestManagedStream_NoSelfInterruptDuringTTS(t *testing.T) {
	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	vad := NewRMSVAD(0.05, 50*time.Millisecond) // Higher threshold for this synthetic test
	conf := DefaultConfig()
	conf.BargeInVADThreshold = 0.05
	orch := NewWithVAD(stt, llm, tts, vad, conf)
	session := NewConversationSession("u3")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	stream.mu.Lock()
	stream.isSpeaking = true
	stream.lastAudioSentAt = time.Now()
	stream.mu.Unlock()

	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		val := int16(819)
		loudChunk[i] = byte(val & 0xFF)
		loudChunk[i+1] = byte(val >> 8)
	}
	for i := 0; i < 20; i++ {
		stream.Write(loudChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type == Interrupted {
			t.Fatal("self-interrupt detected during TTS")
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestManagedStream_TranscriptInterruptWhileSpeaking(t *testing.T) {
	stt := &MockStreamingSTT{steps: []struct {
		text    string
		isFinal bool
		delay   time.Duration
	}{
		{text: "hola", isFinal: false, delay: 150 * time.Millisecond},
	}}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	cfg := DefaultConfig()
	cfg.MinWordsToInterrupt = 1
	vad := NewRMSVAD(0.02, 50*time.Millisecond)
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("u4")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	stream.mu.Lock()
	stream.isSpeaking = true
	stream.mu.Unlock()

	stream.startStreamingSTT(stt)

	select {
	case ev := <-stream.Events():
		if ev.Type != Interrupted {
			t.Fatalf("expected Interrupted from transcript, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for Interrupted via transcript")
	}
}
