package orchestrator

import (
	"context"
	"sync"
	"testing"
)

// TestManagedStream_StaleResolvePendingBargeInDoesNotClobberNewerTurn is a
// regression test for a real bug found while auditing this package's
// generation/turn-tracking logic.
//
// resolvePendingBargeIn's doc comment explicitly states it "No-ops if there
// is no pending barge-in for the current response generation (e.g. ... a
// newer turn has since started)" — pendingBargeGen exists specifically "so a
// resume/confirm can't act on a stale turn" (see its field comment in
// managed_stream.go). But the actual code combined two different cases
// behind one `||`: "no pending barge-in at all" (safe to normalize to Idle)
// and "a pending barge-in exists, but for an older, already-superseded
// generation" (should be a true no-op). Both fell into the same branch,
// which unconditionally forced ms.state = StateIdle — including while a
// NEWER generation's turn was legitimately StateSpeaking or StateProcessing
// right now.
//
// This is reachable in production: resolvePendingBargeIn is called from the
// async processUtterance goroutine (e.g. on isLikelyNoise, an empty
// transcript, or an echo match), which can finish well after a newer turn
// has already started if STT for the old utterance was slow. When that
// stale call landed, it would silently stomp the CURRENT turn's state back
// to Idle — and since emitWithGen's AudioChunk gate checks
// `ms.state == StateSpeaking`, that made the orchestrator silently drop the
// new turn's real, in-flight audio.
//
// Fixed in managed_stream.go by splitting the two cases: only the
// "no pending barge-in" case still forces Idle; a generation mismatch is now
// a true no-op that never touches ms.state.
func TestManagedStream_StaleResolvePendingBargeInDoesNotClobberNewerTurn(t *testing.T) {
	orch := New(&MockSTTProvider{}, &MockLLMProvider{}, &MockTTSProvider{}, DefaultConfig())
	session := NewConversationSession("stale-resolve-test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Simulate: an old utterance (generation 1) tentatively opened a
	// barge-in, but a NEWER turn (generation 2) has since started and is
	// actively speaking right now.
	stream.mu.Lock()
	stream.pendingBargeIn = true
	stream.pendingBargeGen = 1
	stream.payloadGen = 2
	stream.state = StateSpeaking
	stream.mu.Unlock()

	// The old utterance's async goroutine finally reaches its
	// resolvePendingBargeIn call (e.g. isLikelyNoise / empty transcript /
	// echo match resolving a barge-in that's no longer relevant).
	stream.resolvePendingBargeIn()

	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.state != StateSpeaking {
		t.Fatalf("stale resolvePendingBargeIn call for an old generation clobbered the CURRENT (newer) turn's state: got %v, want StateSpeaking — this would silently suppress that turn's real audio (emitWithGen gates AudioChunk on state==StateSpeaking)", stream.state)
	}
	if !stream.pendingBargeIn {
		t.Fatalf("stale resolvePendingBargeIn call cleared pendingBargeIn for a generation it doesn't own — it must be a true no-op for a mismatched generation")
	}
}

// leakTrackingSTT is a StreamingSTTProvider mock that records every audio
// channel it hands back from StreamTranscribe, so a test can independently
// verify whether ManagedStream closed a given session's channel instead of
// abandoning it.
type leakTrackingSTT struct {
	mu       sync.Mutex
	channels []chan []byte
}

func (m *leakTrackingSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	return TranscriptionResult{}, nil
}
func (m *leakTrackingSTT) Name() string { return "LeakTrackingSTT" }
func (m *leakTrackingSTT) StreamTranscribe(ctx context.Context, lang Language, onTranscript func(transcript string, isFinal bool) error) (chan<- []byte, error) {
	ch := make(chan []byte, 8)
	m.mu.Lock()
	m.channels = append(m.channels, ch)
	m.mu.Unlock()
	return ch, nil
}

// TestManagedStream_DuplicateVADStartClosesPriorStreamingSTTSession is a
// regression test for a real resource-leak bug found while auditing this
// package's streaming-STT lifecycle. onVADEnd carefully closes both
// sttResultChan and sttAudioChan when a session ends (its own comment
// describes a prior incident where skipping this leaked a whisper stream's
// goroutine and kv-cache/compute buffers for the rest of a call) — but
// onVADStart, which is what actually creates a new session, had no matching
// guard against starting a SECOND session while a first was already active.
// A duplicate/back-to-back VAD start (a jittery client sending two
// vad_speech_start control frames with no intervening end, or a retriggered
// server-side VAD) simply overwrote ms.sttAudioChan/ms.sttResultChan with a
// fresh pair, orphaning the first session's channels — nothing would ever
// close them, so a real provider's underlying goroutine/connection for that
// first session would run for the rest of the call, compounding exactly the
// leak class onVADEnd's own cleanup exists to prevent, just triggered by
// session-start ordering instead of session-end ordering.
//
// Fixed in onVADStart (managed_stream.go) by closing any already-active
// streaming STT session before starting a new one, mirroring onVADEnd's
// existing cleanup.
func TestManagedStream_DuplicateVADStartClosesPriorStreamingSTTSession(t *testing.T) {
	stt := &leakTrackingSTT{}
	llm := &MockLLMProvider{completeResult: "ok"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1}}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(stt, llm, tts, nil, cfg, &NoOpLogger{})
	session := NewConversationSession("stt-leak-test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	stream.onVADStart(StateIdle)

	stream.mu.Lock()
	firstStarted := stream.sttStarted
	stream.mu.Unlock()
	if !firstStarted {
		t.Fatal("expected the first streaming STT session to start")
	}

	stt.mu.Lock()
	if len(stt.channels) != 1 {
		stt.mu.Unlock()
		t.Fatalf("expected exactly 1 StreamTranscribe call, got %d", len(stt.channels))
	}
	first := stt.channels[0]
	stt.mu.Unlock()

	// Duplicate start with no intervening onVADEnd — simulates a jittery
	// client or a retriggered VAD sending two starts back to back.
	stream.onVADStart(StateListening)

	stt.mu.Lock()
	secondCount := len(stt.channels)
	stt.mu.Unlock()
	if secondCount != 2 {
		t.Fatalf("expected 2 StreamTranscribe calls after the duplicate start, got %d", secondCount)
	}

	select {
	case _, ok := <-first:
		if ok {
			t.Fatal("expected the first session's audio channel to be closed (ok=false), got a value instead")
		}
	default:
		t.Fatal("first streaming STT session's audio channel was not closed on duplicate VAD start — the session leaked")
	}
}
