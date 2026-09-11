package orchestrator

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// blockingAfterFirstTTS delivers audio instantly for one specific
// allow-listed text (so an earlier, unrelated turn can complete normally and
// leave ManagedStream's session-lifetime responseChunksSent counter
// non-zero), then blocks on every other text until its context is
// cancelled, delivering NO audio at all. This lets a test interrupt a later
// turn deterministically before a single byte has been synthesized for it.
//
// Keyed on text rather than call order deliberately: NewManagedStream always
// spawns a background generateBackchannelClips goroutine that independently
// calls this same TTS provider for a few filler phrases ("mhm", "yeah", ...)
// — an earlier, call-count-based version of this mock raced with that
// goroutine and could consume the "first call" slot on a backchannel phrase
// instead of the test's own turn 1, deadlocking the test. Matching on the
// exact response text sidesteps that entirely: the backchannel phrases never
// match a turn's real response text, so they just block harmlessly on
// ms.ctx.Done() until the test's deferred stream.Close() cancels it.
type blockingAfterFirstTTS struct {
	allowText string
}

func (b *blockingAfterFirstTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return nil, nil
}

func (b *blockingAfterFirstTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	if text == b.allowText {
		return onChunk([]byte{1, 2, 3})
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingAfterFirstTTS) Abort() error { return nil }
func (b *blockingAfterFirstTTS) Name() string { return "BlockingAfterFirstTTS" }

// sequencedLLM returns each response in order on successive Complete() calls
// (non-streaming provider — no StreamComplete — matching Anthropic/OpenAI's
// shape and forcing ManagedStream down runLLMAndTTS's non-streaming branch).
type sequencedLLM struct {
	responses []string
	call      int32
}

func (s *sequencedLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	i := atomic.AddInt32(&s.call, 1) - 1
	if int(i) >= len(s.responses) {
		return "", nil
	}
	return s.responses[i], nil
}
func (s *sequencedLLM) Name() string { return "SequencedLLM" }

func waitForEventType(t *testing.T, stream *ManagedStream, want EventType, timeout time.Duration) OrchestratorEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-stream.Events():
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %v", want)
			return OrchestratorEvent{}
		}
	}
}

// TestManagedStream_UnspokenResponseRemovedAfterInterrupt is a regression
// test for a real bug found while auditing this package: ms.responseChunksSent
// (the counter truncateSpokenContext uses to decide "did this response
// actually play any audio before being interrupted", see managed_stream.go's
// truncateSpokenContext) was never reset between turns anywhere in the
// codebase — only ever incremented, in speakText's per-chunk callback. Once
// any turn in a session's lifetime had delivered at least one audio chunk,
// the counter stayed >=1 for the rest of the call. A LATER turn interrupted
// before its own TTS produced a single byte was then wrongly treated as
// "something was spoken" (chunksSent != 0), so the never-spoken assistant
// message for that later turn was left in context instead of being removed
// — the LLM would go on to "remember" saying something the caller never
// heard a single byte of, corrupting later turns' understanding of what was
// actually said.
//
// Fixed by resetting responseChunksSent alongside the neighboring per-turn
// resets in runLLMAndTTS (managed_stream.go) and runStreamingLLM
// (managed_stream_ext.go).
func TestManagedStream_UnspokenResponseRemovedAfterInterrupt(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	llm := &sequencedLLM{responses: []string{"first reply, fully spoken.", "second reply, never spoken."}}
	tts := &blockingAfterFirstTTS{allowText: "first reply, fully spoken."}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(stt, llm, tts, nil, cfg, &NoOpLogger{})
	session := NewConversationSession("ghost-message-test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Turn 1 completes normally end-to-end, delivering one audio chunk. This
	// is what pollutes the session-lifetime responseChunksSent counter
	// without the fix.
	stream.runLLMAndTTS(context.Background(), "hello")

	// Turn 2: the mock TTS blocks before delivering any byte, so we can
	// interrupt deterministically before a single chunk goes out.
	go stream.runLLMAndTTS(context.Background(), "hello again")
	waitForEventType(t, stream, BotResponse, 2*time.Second)

	// Wait for speakText to actually enter the (blocking) TTS call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		ready := stream.ttsCancel != nil
		stream.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	stream.Interrupt()
	waitForEventType(t, stream, Interrupted, 2*time.Second)

	// Give truncateSpokenContext's synchronous work inside handleInterrupt a
	// moment to run (it's on the audioProcessor goroutine, already triggered
	// by the Interrupted event above, but leave a small buffer).
	time.Sleep(50 * time.Millisecond)

	ctx := session.GetContextCopy()
	for _, m := range ctx {
		if m.Role == "assistant" && strings.Contains(m.Content, "never spoken") {
			t.Fatalf("assistant message for a response with zero audio chunks delivered was not removed from context after interrupt: %q", m.Content)
		}
	}
}

// scratchGapStreamingLLM streams "First sentence. " immediately, then sleeps
// to simulate slow token arrival before producing "Second sentence." —
// mirroring a real streaming response where the model keeps generating after
// an early sentence has already gone out to TTS. It deliberately ignores ctx
// cancellation, which is the realistic worst case for a provider whose HTTP
// transport hasn't yet observed a cancelled context (a real, if narrower,
// window most providers have too — this mock just widens it enough to make
// the underlying design gap reproducible on demand instead of only rarely).
type interSentenceGapLLM struct {
	gap time.Duration
}

func (m *interSentenceGapLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return "First sentence. Second sentence.", nil
}
func (m *interSentenceGapLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	if onChunk != nil {
		onChunk("First sentence. ")
	}
	time.Sleep(m.gap)
	if onChunk != nil {
		onChunk("Second sentence.")
	}
	return "First sentence. Second sentence.", nil
}
func (m *interSentenceGapLLM) Name() string { return "InterSentenceGapLLM" }

// TestManagedStream_InterruptDuringInterSentenceGap is a regression test for
// a real bug found during a coverage audit and now fixed: a multi-sentence
// response is synthesized as a separate speakText() call per sentence (see
// runStreamingLLM in managed_stream_ext.go, which drains a ttsQueue channel
// one sentence at a time). Each speakText call independently sets
// ms.state = StateSpeaking on entry and resets it to StateIdle on its own
// completion (managed_stream.go, speakText) — so between sentence 1
// finishing and sentence 2 starting (e.g. while still waiting on more LLM
// tokens), ms.state genuinely sits at StateIdle even though the turn as a
// whole is still very much in flight.
//
// handleInterrupt() used to decide whether to treat a call to Interrupt() as
// a real interruption purely by checking
// `oldState == StateSpeaking || oldState == StateProcessing` at the instant
// it runs — so an interrupt landing in one of these inter-sentence gaps was
// silently swallowed: no Interrupted event, the remaining sentence(s) still
// got synthesized and played, and the full untruncated response still
// landed in session context. Fixed by also checking ms.pipelineCtx (a
// context that stays un-Done for the entire turn, not just the sentence
// currently being spoken) alongside the momentary ms.state.
func TestManagedStream_InterruptDuringInterSentenceGap(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hi"}
	llm := &interSentenceGapLLM{gap: 300 * time.Millisecond}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(stt, llm, tts, nil, cfg, &NoOpLogger{})
	session := NewConversationSession("inter-sentence-gap-test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	go stream.runLLMAndTTS(context.Background(), "hello")

	waitForEventType(t, stream, BotSpeaking, 2*time.Second)

	// Sentence 1 is short and MockTTSProvider is synchronous/instant, so
	// speakText for sentence 1 finishes (and resets state back to Idle)
	// essentially immediately after emitting BotSpeaking — well before
	// sentence 2 arrives ~300ms later. Poll for that Idle landing spot
	// explicitly instead of relying on a fixed sleep to race it correctly,
	// so this test deterministically lands the interrupt IN the gap rather
	// than depending on scheduling luck.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && stream.getState() != StateIdle {
		time.Sleep(2 * time.Millisecond)
	}
	if got := stream.getState(); got != StateIdle {
		t.Fatalf("setup: expected to catch the inter-sentence gap (StateIdle) before sentence 2 arrives, got %v — mock timing assumption no longer holds, this test needs revisiting", got)
	}

	stream.Interrupt()

	// Interrupted must fire even though the interrupt landed while
	// ms.state read Idle (the gap between sentence 1 and sentence 2).
	waitForEventType(t, stream, Interrupted, 1*time.Second)

	// The rest of the turn must NOT play out: sentence 2 never gets
	// synthesized/appended once the pipeline context is cancelled, so it
	// must never appear in session context.
	time.Sleep(500 * time.Millisecond) // longer than interSentenceGapLLM's 300ms gap
	ctx := session.GetContextCopy()
	for _, m := range ctx {
		if m.Role == "assistant" && strings.Contains(m.Content, "Second sentence") {
			t.Fatalf("unspoken second sentence landed in context after interrupt — the inter-sentence-gap drop bug has regressed: %q", m.Content)
		}
	}
}
