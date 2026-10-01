package orchestrator

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedLLM answers each call with its last user message, once released, and records tokens on the
// context's sink as a provider would.
type gatedLLM struct {
	calls   atomic.Int32
	release chan struct{}
}

func (g *gatedLLM) Complete(ctx context.Context, messages []Message, _ []Tool) (string, error) {
	g.calls.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	TokenUsageFrom(ctx).Record(100, 10, 110)
	return "reply to " + messages[len(messages)-1].Content, nil
}

func (g *gatedLLM) Name() string { return "gated" }

func newSpecOrch(llm LLMProvider) *Orchestrator {
	return NewWithAllLayers(nil, llm, nil, nil, DefaultConfig(), &NoOpLogger{})
}

// A seed on newer words replaces a guess on older ones instead of being refused: the refusal kept
// the transcript the turn would use from ever being speculated on (87% of turns, 2026-10-01).
func TestSeedOnNewWordsReplacesStaleGuess(t *testing.T) {
	llm := &gatedLLM{release: make(chan struct{})}
	orch := newSpecOrch(llm)
	se := NewSpeculativeExecutor(100)
	var mu sync.Mutex
	var runs []SpeculativeRun
	se.SetOnFinish(func(r SpeculativeRun) { mu.Lock(); runs = append(runs, r); mu.Unlock() })

	se.StartFromTranscript(context.Background(), orch, "quiero una", nil, nil)
	se.StartFromTranscript(context.Background(), orch, "quiero una demo el viernes", nil, nil)
	close(llm.release)

	resp, ok := se.Await(context.Background(), "Quiero una demo el viernes.")
	if !ok || !strings.Contains(resp, "viernes") {
		t.Fatalf("the newer seed should answer: %q %v", resp, ok)
	}
	if p, c, _, ok := se.ResultTokens().Snapshot(); !ok || p != 100 || c != 10 {
		t.Fatalf("result tokens %d/%d %v", p, c, ok)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	outcomes := map[string]int{}
	for _, r := range runs {
		outcomes[r.Outcome]++
	}
	if outcomes["cancelled"] != 1 || outcomes["responded"] != 1 {
		t.Fatalf("want the stale run cancelled and the new one responded, got %v", outcomes)
	}
}

// The same words seeded twice (each hangover frame re-seeds) cost one call.
func TestSeedOnSameWordsIsOneCall(t *testing.T) {
	llm := &gatedLLM{release: make(chan struct{})}
	orch := newSpecOrch(llm)
	se := NewSpeculativeExecutor(100)
	se.StartFromTranscript(context.Background(), orch, "hola, ¿qué tal?", nil, nil)
	se.StartFromTranscript(context.Background(), orch, "Hola qué tal", nil, nil)
	close(llm.release)
	if _, ok := se.Await(context.Background(), "hola qué tal"); !ok {
		t.Fatal("expected a hit")
	}
	if n := llm.calls.Load(); n != 1 {
		t.Fatalf("%d model calls for the same words, want 1", n)
	}
}

// A cancelled run that finishes late cannot overwrite the run that replaced it.
func TestLateFinishDoesNotOverwrite(t *testing.T) {
	slow := &gatedLLM{release: make(chan struct{})}
	orch := newSpecOrch(slow)
	se := NewSpeculativeExecutor(100)
	se.StartFromTranscript(context.Background(), orch, "primera", nil, nil)
	se.Cancel()
	se.StartFromTranscript(context.Background(), orch, "segunda", nil, nil)
	close(slow.release)
	resp, ok := se.Await(context.Background(), "segunda")
	if !ok || !strings.Contains(resp, "segunda") {
		t.Fatalf("got %q %v", resp, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if resp, ok := se.Await(context.Background(), "segunda"); !ok || !strings.Contains(resp, "segunda") {
		t.Fatalf("a late finisher overwrote the result: %q %v", resp, ok)
	}
}

// silenceAwareVAD is an RMS VAD that also reports silence frames, as SileroVAD does in production,
// which is what turns the hangover seed on.
type silenceAwareVAD struct{ *RMSVAD }

func (v silenceAwareVAD) SilenceFrames() int { return 0 }

// Clone keeps the silence reporting: each stream gets its own copy of the VAD.
func (v silenceAwareVAD) Clone() VADProvider {
	return silenceAwareVAD{v.RMSVAD.Clone().(*RMSVAD)}
}

// With the hangover seed active, a pause mid-sentence makes no model call: the audio-triggered guess
// it used to start was used on 5-10% of turns and kept the seed from running (2026-10-01).
func TestPauseMakesNoModelCallWhenTheSeedIsActive(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello there"}
	llm := &countingLLM{result: "General Kenobi"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := silenceAwareVAD{NewRMSVAD(0.1, 500*time.Millisecond)}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 8000
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("test"))
	defer stream.Close()

	loud := make([]byte, 100)
	for i := 0; i < len(loud); i += 2 {
		loud[i], loud[i+1] = 0xFF, 0x7F
	}
	quiet := make([]byte, 100)
	for i := 0; i < 60; i++ {
		stream.Write(loud)
	}
	time.Sleep(50 * time.Millisecond)
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
		stream.Write(quiet)
		time.Sleep(20 * time.Millisecond)
	}
	if got := llm.calls.Load(); got != 0 {
		t.Fatalf("a mid-sentence pause called the model %d times", got)
	}
}

// The seed, the run's cost line and a hit carry one run number, so each paid run ties to its outcome.
func TestRunIDTiesRunToResult(t *testing.T) {
	llm := &gatedLLM{release: make(chan struct{})}
	orch := newSpecOrch(llm)
	se := NewSpeculativeExecutor(400)
	var finished []SpeculativeRun
	var mu sync.Mutex
	se.SetOnFinish(func(r SpeculativeRun) { mu.Lock(); finished = append(finished, r); mu.Unlock() })

	se.StartFromTranscript(context.Background(), orch, "primera", nil, nil)
	first := se.RunID()
	se.StartFromTranscript(context.Background(), orch, "segunda", nil, nil)
	second := se.RunID()
	close(llm.release)
	if first == 0 || second != first+1 {
		t.Fatalf("run numbers: %d then %d", first, second)
	}
	if _, ok := se.Await(context.Background(), "segunda"); !ok {
		t.Fatalf("expected the second run to answer")
	}
	if got := se.ResultRunID(); got != second {
		t.Fatalf("result run %d, want %d", got, second)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	ids := map[int]string{}
	for _, r := range finished {
		ids[r.ID] = r.Outcome
	}
	if ids[first] != "cancelled" || ids[second] != "responded" {
		t.Fatalf("cost lines by run: %v", ids)
	}
}
