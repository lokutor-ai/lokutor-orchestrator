package orchestrator

import (
	"context"
	"testing"
	"time"
)

// eouSTT answers every utterance with text and, when has is set, the recogniser's end-of-turn probability.
type eouSTT struct {
	text string
	eou  float64
	has  bool
}

func (s eouSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	return TranscriptionResult{Text: s.text, EndOfTurn: s.eou, HasEndOfTurn: s.has}, nil
}
func (s eouSTT) Name() string { return "eou" }

// eouTurnWait runs one utterance through processUtterance and reports how long it waited for the caller to go on.
func eouTurnWait(t *testing.T, stt eouSTT, threshold float64) int64 {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SilenceConfirmationMs = 300
	cfg.EndOfTurnThreshold = threshold
	cfg.EndOfTurnWaitMs = 200
	orch := NewWithAllLayers(stt, &sequencedLLM{responses: []string{"Claro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("eou"))
	defer stream.Close()
	stream.mu.Lock()
	stream.state = StateProcessing
	stream.utteranceSeq = 1
	stream.inflightUtterances = 1
	stream.mu.Unlock()
	stream.processUtterance(make([]byte, 64000), 2*time.Second, 1)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.confirmWaitMs
}

// The recogniser ends every transcript with a full stop, so "...an appointment for." reads as finished and was
// answered after the VAD hangover alone. With a threshold its probability decides: low waits for the caller to go on
// (EndOfTurnWaitMs), high answers at once even when the words trail off; and with none set nothing changes.
func TestEndOfTurnProbabilityDecidesTheWait(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stt       eouSTT
		threshold float64
		wantWait  bool
	}{
		{"low probability waits though the text ends in a full stop", eouSTT{"I'd like to book an appointment for.", 0.1, true}, 0.7, true},
		{"high probability answers though the words trail off", eouSTT{"I want to book a table, and", 0.95, true}, 0.7, false},
		{"shadow: no threshold leaves the full stop's verdict", eouSTT{"I'd like to book an appointment for.", 0.1, true}, 0, false},
		{"no probability falls back to the lexical gate", eouSTT{"I want to book a table, and", 0, false}, 0.7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waited := eouTurnWait(t, tc.stt, tc.threshold)
			if tc.wantWait && waited < 150 {
				t.Fatalf("waited %d ms, want the confirmation wait", waited)
			}
			if !tc.wantWait && waited != 0 {
				t.Fatalf("waited %d ms, want an answer at once", waited)
			}
		})
	}
}
