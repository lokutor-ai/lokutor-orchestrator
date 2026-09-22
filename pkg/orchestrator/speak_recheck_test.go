package orchestrator

import (
	"context"
	"testing"
	"time"
)

type timeT = time.Time

func timeNowForTest() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) }

// noopSpeakLogger keeps the guard's log line out of test output.
type noopSpeakLogger struct{}

func (noopSpeakLogger) Debug(string, ...interface{}) {}
func (noopSpeakLogger) Info(string, ...interface{})  {}
func (noopSpeakLogger) Warn(string, ...interface{})  {}
func (noopSpeakLogger) Error(string, ...interface{}) {}

// onVADEnd removes the pre-generation wait on the explicit promise that a late
// recheck catches a caller who resumed talking "right before speakText plays
// anything". That recheck did not exist, so a caller who started speaking again
// during generation was spoken over: no bot audio existed yet, so nothing
// registered as a barge-in and playback simply began on top of them.
func TestSpeakTextDiscardsResponseWhenCallerResumed(t *testing.T) {
	ms := &ManagedStream{
		orch:   &Orchestrator{config: Config{}},
		logger: noopSpeakLogger{},
		state:  StateProcessing,
	}
	ms.vadSpeaking = true // the caller started talking again during generation

	ms.speakText(context.Background(), "a response they have stopped waiting for", 0)

	if ms.state == StateSpeaking {
		t.Error("began playback while the caller was speaking")
	}
	if ms.state != StateListening {
		t.Errorf("state = %v, want StateListening so the new utterance is handled", ms.state)
	}
	if !ms.botSpeakStart.IsZero() {
		t.Error("playback timing was started despite discarding the response")
	}
}

// The guard must not make the agent mute: with the caller silent, the normal
// path still has to reach playback.
func TestSpeakTextProceedsWhenCallerSilent(t *testing.T) {
	ms := &ManagedStream{
		orch:   &Orchestrator{config: Config{}},
		logger: noopSpeakLogger{},
		state:  StateProcessing,
	}
	ms.vadSpeaking = false

	// A cancelled context stops it before real TTS work, but only *after* the
	// vadSpeaking guard — so reaching the context check proves the guard let
	// it through rather than swallowing the turn.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ms.speakText(ctx, "hello", 0)

	if ms.state == StateListening {
		t.Error("a silent caller must not cause the response to be discarded as a resume")
	}
}

// The client SDK treats a "thinking" status message as authoritative: on seeing a higher
// generation number it bumps its own currentGeneration AND stops whatever audio is currently
// playing immediately, with no way to undo that if the generation it was just told about gets
// discarded a moment later. BotThinking must therefore never reach the client for a response that
// speakText goes on to discard here — see the comment on the emission itself for the full history
// (this used to fire at generation-allocation time in runLLMAndTTS/trySpeculativeResponse/the
// tool-call continuation goroutine, cutting off real, valid, still-playing audio for a generation
// that then turned out to be abandoned, which is what showed up in production as the bot
// restarting itself mid-sentence).
func TestSpeakTextDoesNotEmitBotThinkingWhenDiscarded(t *testing.T) {
	ms := &ManagedStream{
		orch:   &Orchestrator{config: Config{}},
		logger: noopSpeakLogger{},
		state:  StateProcessing,
		events: make(chan OrchestratorEvent, 8),
	}
	ms.vadSpeaking = true // the caller started talking again during generation

	ms.speakText(context.Background(), "a response they have stopped waiting for", 1)

	select {
	case ev := <-ms.events:
		t.Fatalf("discarded response must emit nothing the client could act on, got %v", ev.Type)
	default:
	}
}

// The other half of the same fix: a response that genuinely proceeds to playback must still tell
// the client a new generation exists, or the UI never leaves its "thinking" state and the client's
// own generation bookkeeping falls behind the server's. Runs the real pipeline (proper
// constructor, mock providers) rather than calling speakText directly, since a live TTS call is
// exactly what has to succeed here — a hand-cancelled context racing the guard would either miss
// the assertion or reach a nil provider first.
func TestManagedStream_EmitsBotThinkingWhenResponseProceeds(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello there"}
	llm := &MockLLMProvider{completeResult: "hi, how can I help"}
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	vad := NewRMSVAD(0.05, 50*time.Millisecond)
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("thinking-timing"))
	defer stream.Close()

	stream.processUtterance([]byte{0, 0, 0, 0}, 1*time.Second, 0)

	sawThinking := false
	deadline := time.After(1 * time.Second)
	for !sawThinking {
		select {
		case ev := <-stream.Events():
			if ev.Type == BotThinking {
				sawThinking = true
			}
		case <-deadline:
			t.Fatal("expected a BotThinking event for a response that was never discarded")
		}
	}
}

// stageMs must distinguish "not measured" from "instant". A zero would read as
// a stage that took no time, which is exactly the wrong conclusion to draw
// when optimising.
func TestStageMsReportsUnmeasuredAsNegative(t *testing.T) {
	base := timeNowForTest()
	cases := []struct {
		name     string
		from, to timeT
		want     int64
	}{
		{"both unset", timeT{}, timeT{}, -1},
		{"from unset", timeT{}, base, -1},
		{"to unset", base, timeT{}, -1},
		{"reversed", base.Add(100 * time.Millisecond), base, -1},
		{"measured", base, base.Add(250 * time.Millisecond), 250},
	}
	for _, tc := range cases {
		if got := stageMs(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: stageMs = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSpeakTextDefersLastActivityToEstimatedPlaybackEnd is a regression test for a real production
// bug: lastActivityAt used to be set to time.Now() the moment speakText finished GENERATING and
// SENDING a turn's audio, not the moment the client would actually FINISH PLAYING it. Synthesis
// runs faster than real time, so for a long response the server can finish sending well before the
// client finishes playing it back — during that gap monitorInactivity's silence-timeout nudge saw
// an "idle" stream that was, from the caller's side, still mid-sentence, and spoke its own "want
// more?" reply on top of the real one. Reported repeatedly in production as the bot interrupting
// and restarting itself, always on long, multi-sentence replies.
//
// This uses the real pipeline (NewWithVAD + NewManagedStream, a mock TTS provider) rather than a
// bare struct literal, because the fix lives in how speakText derives lastActivityAt from actual
// bytes sent through the real onChunk callback — a hand-set field would not exercise it.
func TestSpeakTextDefersLastActivityToEstimatedPlaybackEnd(t *testing.T) {
	const playbackRate = 44100
	const audioSeconds = 10
	audio := make([]byte, playbackRate*2*audioSeconds) // 16-bit PCM, mono, 10s of silence

	stt := &MockSTTProvider{}
	llm := &MockLLMProvider{}
	tts := &MockTTSProvider{synthesizeResult: audio}
	vad := NewRMSVAD(0.05, 50*time.Millisecond)
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("playback-defer-test"))
	defer stream.Close()

	if stream.playbackRate != playbackRate {
		t.Fatalf("test assumes playbackRate %d, got %d", playbackRate, stream.playbackRate)
	}

	before := time.Now()
	stream.speakText(context.Background(), "a long response", 1)
	after := time.Now()

	stream.mu.Lock()
	got := stream.lastActivityAt
	stream.mu.Unlock()

	minExpected := before.Add(audioSeconds * time.Second)
	if got.Before(minExpected) {
		t.Fatalf("lastActivityAt = %v, want at least %v (%ds after speakText started) — the silence "+
			"timeout would fire while the client is still playing this response back",
			got, minExpected, audioSeconds)
	}
	// A loose upper bound: it must not have been pushed absurdly far out either (e.g. by a units
	// bug turning seconds into something much larger).
	if maxExpected := after.Add(audioSeconds * time.Second); got.After(maxExpected) {
		t.Fatalf("lastActivityAt = %v is implausibly far in the future (want at most ~%v)", got, maxExpected)
	}
}
