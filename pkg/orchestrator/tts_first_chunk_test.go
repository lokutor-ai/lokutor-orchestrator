package orchestrator

import (
	"context"
	"testing"
	"time"
)

// gappedTwoSentenceLLM streams "First sentence." immediately (queued and
// synthesized right away), then simulates slow LLM token arrival before
// the second sentence shows up — mirroring a real streaming response where
// the first sentence's audio is already playing well before the model has
// even produced the rest. The bug this is designed to catch overwrites
// ttsFirstChunkTime at the START of the SECOND sentence's speakText call,
// which only happens after this gap — so the gap is what makes the buggy
// and correct values clearly distinguishable in a test.
type gappedTwoSentenceLLM struct{}

func (m *gappedTwoSentenceLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "First sentence. Second sentence.", nil
}

func (m *gappedTwoSentenceLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	if onChunk != nil {
		onChunk("First sentence. ")
	}
	time.Sleep(250 * time.Millisecond) // simulated slow token arrival gap
	if onChunk != nil {
		onChunk("Second sentence.")
	}
	return "First sentence. Second sentence.", nil
}
func (m *gappedTwoSentenceLLM) Name() string { return "GappedTwoSentenceLLM" }

// fastTTS finishes near-instantly — isolates the LLM-side gap above as the
// only meaningful delay in this test.
type fastTTS struct{}

func (t *fastTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return []byte{1}, nil
}

func (t *fastTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	return onChunk([]byte{1, 2, 3})
}

func (t *fastTTS) Abort() error { return nil }
func (t *fastTTS) Name() string { return "FastTTS" }

// TestTTSFirstChunkTime_ReflectsFirstSentenceNotLast is the regression test
// for the bug where speakText unconditionally overwrote ms.ttsFirstChunkTime
// on every call, so a multi-sentence response's reported "time to first
// audio" actually measured the LAST sentence's start time — inflating
// ttfa_ms/tts_first_ms by however long the earlier sentences took to
// finish, on every multi-sentence reply.
func TestTTSFirstChunkTime_ReflectsFirstSentenceNotLast(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	llm := &gappedTwoSentenceLLM{}
	tts := &fastTTS{}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	base := time.Now()
	stream.userSpeechEnd = base

	stream.runLLMAndTTS(context.Background(), "hello")

	if stream.ttsFirstChunkTime.IsZero() {
		t.Fatal("expected ttsFirstChunkTime to be set")
	}
	elapsed := stream.ttsFirstChunkTime.Sub(base)

	// The first sentence is ready and synthesized almost immediately; the
	// second only shows up after a simulated 250ms LLM gap. A correct
	// measurement lands close to the first sentence's real completion
	// (well under 100ms); the pre-fix bug reset ttsFirstChunkTime at the
	// START of the second sentence's speakText call, which only happens
	// after that 250ms gap.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("ttsFirstChunkTime measured %v after turn start — looks like it captured the LAST sentence's start (post-gap), not the first sentence's real completion (bug regression)", elapsed)
	}
}
