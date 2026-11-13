package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"
)

// visemeTimedTTS says every segment as `secs` of audio at 16 kHz and gives one viseme per segment at 0.1-0.2 s of it.
type visemeTimedTTS struct {
	MockTTSProvider
	secs float64
}

func (t *visemeTimedTTS) StreamSynthesizeVisemes(ctx context.Context, text string, voice Voice, lang Language,
	onChunk func([]byte) error, onVisemes func([]Viseme)) error {
	onVisemes([]Viseme{{ID: 10, Name: "aa", Start: 0.1, End: 0.2}})
	return onChunk(make([]byte, int(t.secs*16000)*2))
}

func collectVisemes(stream *ManagedStream, wait time.Duration) (got []Viseme, gen int) {
	deadline := time.After(wait)
	for {
		select {
		case ev := <-stream.Events():
			if ev.Type == Visemes {
				got = append(got, ev.Data.([]Viseme)...)
				gen = ev.Generation
			}
		case <-deadline:
			return got, gen
		}
	}
}

// Each segment's mouth shapes reach the client as a Visemes event of the reply's generation, moved by the audio the
// reply spoke before that segment, so they line up with what the client plays; nothing without SetVisemes.
func TestVisemesAreTimedFromTheReplysFirstSample(t *testing.T) {
	for _, on := range []bool{true, false} {
		stt := &MockSTTProvider{transcribeResult: "hi"}
		llm := &sequencedLLM{responses: []string{"The first segment of the reply."}}
		cfg := DefaultConfig()
		cfg.SilenceTimeout = 0
		orch := NewWithAllLayers(stt, llm, &visemeTimedTTS{secs: 0.5}, nil, cfg, &NoOpLogger{})
		stream := orch.NewManagedStream(context.Background(), NewConversationSession("visemes-test"))
		stream.SetPlaybackRate(16000)
		stream.SetVisemes(on)
		stream.runLLMAndTTS(context.Background(), "hello")
		first, gen := collectVisemes(stream, 300*time.Millisecond)
		if !on {
			stream.Close()
			if len(first) != 0 {
				t.Fatalf("visemes without SetVisemes: %v", first)
			}
			continue
		}
		if len(first) != 1 || math.Abs(first[0].Start-0.1) > 1e-9 {
			t.Fatalf("first segment: %+v", first)
		}
		// the reply's next segment, in the same turn (as speakResponse and the streamed path speak them)
		stream.mu.Lock()
		stream.state = StateProcessing
		stream.mu.Unlock()
		stream.speakText(context.Background(), "And a second segment after it.", gen)
		second, gen2 := collectVisemes(stream, 300*time.Millisecond)
		stream.Close()
		if gen2 != gen || len(second) != 1 {
			t.Fatalf("second segment: %+v (generation %d vs %d)", second, gen2, gen)
		}
		if math.Abs(second[0].Start-0.6) > 1e-9 || math.Abs(second[0].End-0.7) > 1e-9 {
			t.Fatalf("not moved by the 0.5 s spoken before it: %+v", second[0])
		}
	}
}
