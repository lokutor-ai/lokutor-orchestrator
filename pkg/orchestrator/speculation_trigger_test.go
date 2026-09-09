package orchestrator

import (
	"context"
	"testing"
	"time"
)

// TestSpeculation_FastPauseTriggersLLMBeforeHangoverConfirms is the
// end-to-end proof for the fast pause-trigger: with the main VAD's
// hangover set deliberately slow (500ms) and the fixed pause-trigger delay
// at 100ms (speculation_trigger.go's pauseSpecDelay), the LLM must already
// have been called well before 500ms of silence has even been sent — i.e.
// speculative generation actually started during the pause, not only
// after the real end-of-turn was confirmed. MockSTTProvider returns the
// same fixed transcript regardless of input audio, so the speculative and
// final transcripts always match here, letting the shortcut engage.
func TestSpeculation_FastPauseTriggersLLMBeforeHangoverConfirms(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello there"}
	llm := &countingLLM{result: "General Kenobi"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 500*time.Millisecond) // slow hangover on purpose

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 8000               // keeps the required speculation-worthy audio buffer small
	cfg.FirstSpeaker = FirstSpeakerUser // avoid the default bot-greeting flow calling the LLM independently
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	loudChunk := make([]byte, 100)
	for i := 0; i < len(loudChunk); i += 2 {
		loudChunk[i] = 0xFF
		loudChunk[i+1] = 0x7F
	}
	quietChunk := make([]byte, 100)

	// Enough loud audio to (a) confirm speech-start and (b) clear
	// minSpecAudioMs's byte threshold at 8kHz.
	for i := 0; i < 60; i++ {
		stream.Write(loudChunk)
	}
	time.Sleep(50 * time.Millisecond) // let the audio processor catch up

	// Cross the 100ms pause-trigger delay, but stay well under the VAD's
	// 500ms hangover — nothing should have confirmed end-of-turn yet.
	quietDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(quietDeadline) {
		stream.Write(quietChunk)
		time.Sleep(20 * time.Millisecond)
	}

	if got := llm.calls.Load(); got != 1 {
		t.Fatalf("expected the speculative trigger to have called the LLM exactly once by t+200ms (well before the 500ms hangover), got %d calls", got)
	}

	// Now let the real hangover actually fire and the full pipeline run.
	finalDeadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(finalDeadline) {
		stream.Write(quietChunk)
		time.Sleep(20 * time.Millisecond)
	}

	if got := llm.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 total LLM call (speculative result reused, no redundant real call), got %d", got)
	}
}
