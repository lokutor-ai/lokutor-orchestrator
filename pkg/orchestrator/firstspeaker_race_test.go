package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

// delayedSTTProvider sleeps before returning, standing in for whatever made
// production's own real-utterance pipeline take a long time to reach
// runLLMAndTTS (that turn's own pre-LLM gate measured over sixteen seconds,
// with every finer-grained checkpoint inside it reading near zero -- the
// delay was somewhere between STT finishing and the LLM call starting, not
// inside any one instrumented step). Standing in for that whole span here
// means the real utterance's own runLLMAndTTS call -- and that call's
// pre-existing "cancel whatever pipeline is already running" check -- has not
// even happened yet when the test looks at whether the opening got through,
// which is what isolates realUtteranceStarted's own, earlier cancellation in
// onVADEnd as the thing actually being tested.
type delayedSTTProvider struct {
	result string
	delay  time.Duration
}

func (d *delayedSTTProvider) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return TranscriptionResult{}, ctx.Err()
	}
	return TranscriptionResult{Text: d.result}, nil
}

func (d *delayedSTTProvider) Name() string { return "delayedSTT" }

// openingLLMProvider answers FirstSpeakerBot's own opening call (its trailing
// message is always OpeningTrigger, since this agent has no configured
// OpeningMessage) after a short delay, standing in for production's own
// measured 257ms. Any other call -- the real utterance's own -- answers
// immediately; this test never needs it to be slow, since delayedSTTProvider
// already keeps the real turn from reaching runLLMAndTTS at all during the
// window this test inspects.
type openingLLMProvider struct {
	openingStarted chan struct{}
	openingDelay   time.Duration
	once           sync.Once
}

func (r *openingLLMProvider) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	if len(messages) > 0 && messages[len(messages)-1].Content == OpeningTrigger {
		r.once.Do(func() { close(r.openingStarted) })
		select {
		case <-time.After(r.openingDelay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "stale generic opening", nil
	}
	return "real answer to the real question", nil
}

func (r *openingLLMProvider) Name() string { return "openingLLM" }

// recordingTTSProvider records the text of every synthesis request, so a test
// can assert what was actually sent to TTS -- not just what events the
// pipeline emitted, since BotResponse's text going to the transcript panel
// and speakText actually reaching TTS are two different things (see the
// realUtteranceStarted field comment).
type recordingTTSProvider struct {
	mu    sync.Mutex
	texts []string
}

func (r *recordingTTSProvider) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return []byte("audio"), nil
}

func (r *recordingTTSProvider) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
	return onChunk([]byte("audio"))
}

func (r *recordingTTSProvider) Abort() error { return nil }
func (r *recordingTTSProvider) Name() string { return "recordingTTS" }

func (r *recordingTTSProvider) sawText(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.texts {
		if t == want {
			return true
		}
	}
	return false
}

func (r *recordingTTSProvider) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.texts))
	copy(out, r.texts)
	return out
}

// TestFirstSpeakerBot_OpeningAbandonedWhenRealUtteranceArrivesFirst reproduces
// the production bug: a caller who starts talking before FirstSpeakerBot's own
// opening (no verbatim OpeningMessage configured, so it falls to an LLM call
// on the OpeningTrigger instruction) finishes its own LLM round trip must
// never hear that stale opening. Before the realUtteranceStarted guard, the
// opening flow and the real per-utterance flow ran independently: the
// opening's LLM call, already in flight and fast, would reach speakText and
// play in full while the caller's real answer was still being worked on
// (production's own pre-LLM gate for that turn ran over sixteen seconds), and
// whichever one finished last would cut off and replace the other -- which is
// what the production reports described as the bot answering, restarting,
// and answering again for one utterance.
func TestFirstSpeakerBot_OpeningAbandonedWhenRealUtteranceArrivesFirst(t *testing.T) {
	llm := &openingLLMProvider{
		openingStarted: make(chan struct{}),
		openingDelay:   20 * time.Millisecond,
	}
	// The real utterance's own STT is slow, so processUtterance -> runLLMAndTTS
	// (and that call's own, pre-existing pipeline-cancel check) has not even
	// started by the time the fast opening would otherwise finish and speak.
	stt := &delayedSTTProvider{result: "tell me a long story", delay: 300 * time.Millisecond}
	tts := &recordingTTSProvider{}
	vad := NewRMSVAD(0.05, 50*time.Millisecond)
	cfg := DefaultConfig() // FirstSpeaker defaults to FirstSpeakerBot; no OpeningMessage configured
	cfg.SilenceTimeout = 0
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("firstspeaker-race"))
	defer stream.Close()

	stream.NotifyTransportReady()

	select {
	case <-llm.openingStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("opening flow's LLM call never started")
	}

	// Simulate a real utterance confirming while the opening's LLM call is
	// still in flight -- mirrors onVADEnd's real preconditions (userSpeakingSince
	// far enough in the past to clear minDur, userAudio long enough to clear minLen).
	stream.mu.Lock()
	stream.userSpeakingSince = time.Now().Add(-300 * time.Millisecond)
	stream.userAudio = make([]byte, 3200)
	stream.mu.Unlock()
	stream.onVADEnd(StateListening)

	// Give the (fast) opening plenty of time to reach TTS if nothing stops it,
	// well before the (slow) real answer is anywhere near ready.
	time.Sleep(llm.openingDelay + 150*time.Millisecond)
	if tts.sawText("stale generic opening") {
		t.Fatal("the stale opening reached TTS after a real utterance had already started -- the caller would have heard it")
	}

	deadline := time.After(2 * time.Second)
	for !tts.sawText("real answer to the real question") {
		select {
		case <-stream.Events():
		case <-deadline:
			t.Fatalf("timed out waiting for the real answer to be spoken; TTS saw: %#v", tts.snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}

	if tts.sawText("stale generic opening") {
		t.Error("the stale opening reached TTS at some point during the turn -- the caller would have heard it")
	}
}
