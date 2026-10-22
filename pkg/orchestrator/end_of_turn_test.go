package orchestrator

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// eouSTT answers every utterance with text and, when has is set, the recogniser's end-of-turn probability.
type eouSTT struct {
	text string
	eou  float64
	has  bool
	alt  *float64
}

func (s eouSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	r := TranscriptionResult{Text: s.text, EndOfTurn: s.eou, HasEndOfTurn: s.has}
	if s.alt != nil {
		r.EndOfTurnAlt, r.HasEndOfTurnAlt = *s.alt, true
	}
	return r, nil
}
func (s eouSTT) Name() string { return "eou" }

// eouTurnWait runs one utterance through processUtterance and reports how long it waited for the caller to go on.
func eouTurnWait(t *testing.T, stt eouSTT, threshold float64, useAlt ...bool) int64 {
	t.Helper()
	cfg := DefaultConfig()
	cfg.EndOfTurnUseAlt = len(useAlt) > 0 && useAlt[0]
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
		{"low probability waits though the text ends in a full stop", eouSTT{"I'd like to book an appointment for.", 0.1, true, nil}, 0.7, true},
		{"high probability answers though the words trail off", eouSTT{"I want to book a table, and", 0.95, true, nil}, 0.7, false},
		{"shadow: no threshold leaves the full stop's verdict", eouSTT{"I'd like to book an appointment for.", 0.1, true, nil}, 0, false},
		{"no probability falls back to the lexical gate", eouSTT{"I want to book a table, and", 0, false, nil}, 0.7, true},
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

// With two heads scored, EndOfTurnUseAlt picks the one that decides; the other is only logged.
func TestEndOfTurnAltDecidesWhenChosen(t *testing.T) {
	low, high := 0.1, 0.95
	text := "I'd like to book an appointment for."
	if w := eouTurnWait(t, eouSTT{text, 0.95, true, &low}, 0.7, true); w < 150 {
		t.Fatalf("alt says unfinished but waited %d ms", w)
	}
	if w := eouTurnWait(t, eouSTT{text, 0.1, true, &high}, 0.7, true); w != 0 {
		t.Fatalf("alt says finished but waited %d ms", w)
	}
	if w := eouTurnWait(t, eouSTT{text, 0.95, true, &low}, 0.7); w != 0 {
		t.Fatalf("eou decides by default: finished, yet waited %d ms", w)
	}
}

// A turn the end-of-turn wait held is answered whatever its continuation turns out to be. The caller resumed
// with "hours", the recogniser returned nothing for it alone (or a filler), and discarding that as noise used to drop
// the held "Can you tell me your opening" too: the caller got silence (2026-10-22, live, with EOU_THRESHOLD on).
func TestHeldTurnIsAnsweredWhenItsContinuationIsEmpty(t *testing.T) {
	for _, cont := range []string{"", "Mm."} {
		t.Run("continuation "+cont, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.SilenceTimeout = 0
			cfg.SilenceConfirmationMs = 200
			llm := &sequencedLLM{responses: []string{"We open at nine."}}
			orch := NewWithAllLayers(eouSTT{cont, 0, false, nil}, llm, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
			stream := orch.NewManagedStream(context.Background(), NewConversationSession("held"))
			defer stream.Close()
			stream.mu.Lock()
			stream.carry = &committedUtterance{audio: make([]byte, 32000), transcript: "Can you tell me your opening",
				endedAt: time.Now(), gen: stream.payloadGen, seq: 1}
			stream.state = StateProcessing
			stream.utteranceSeq = 2
			stream.inflightUtterances = 1
			stream.mu.Unlock()
			stream.processUtterance(make([]byte, 8000), 250*time.Millisecond, 2)
			if atomic.LoadInt32(&llm.call) == 0 {
				t.Fatal("the held turn was never answered")
			}
			stream.session.mu.Lock()
			defer stream.session.mu.Unlock()
			found := false
			for _, m := range stream.session.Context {
				if m.Role == "user" && strings.Contains(m.Content, "opening") {
					found = true
				}
			}
			if !found {
				t.Fatalf("the held words are not in the conversation: %+v", stream.session.Context)
			}
		})
	}
}

// The caller resumed while the end-of-turn wait held their turn, so the stream was Processing and onVADStart opened a
// tentative barge-in for the resumption. Its continuation was one word ("11.", 180 ms): the word gate counted it alone,
// called it a too-short barge-in, and the false alarm dropped the held "How does Locke compared to?" with it. No reply,
// nothing to resume, and the silence check-in ~10 s later (2026-10-22, live, a sentence the VAD split in two). The gate
// counts the words of the turn being answered.
func TestHeldTurnContinuationIsNotJudgedAsAOneWordBargeIn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SilenceConfirmationMs = 200
	llm := &sequencedLLM{responses: []string{"Lokutor runs its own models."}}
	orch := NewWithAllLayers(eouSTT{"11.", 0.9, true, nil}, llm, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("held-barge"))
	defer stream.Close()
	stream.mu.Lock()
	stream.carry = &committedUtterance{audio: make([]byte, 32000), transcript: "How does Locke compared to?",
		endedAt: time.Now(), gen: stream.payloadGen, seq: 1}
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.state = StateListening
	stream.utteranceSeq = 2
	stream.inflightUtterances = 1
	stream.mu.Unlock()
	stream.processUtterance(make([]byte, 5760), 180*time.Millisecond, 2)
	if atomic.LoadInt32(&llm.call) == 0 {
		t.Fatal("the held turn was dropped as a one-word barge-in")
	}
	stream.session.mu.Lock()
	defer stream.session.mu.Unlock()
	found := false
	for _, m := range stream.session.Context {
		if m.Role == "user" && strings.Contains(m.Content, "compared") && strings.Contains(m.Content, "11") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the turn answered is not the two halves together: %+v", stream.session.Context)
	}
}

// A one-word utterance over the agent that continues nothing is still a false alarm: the gate's purpose is unchanged.
func TestOneWordBargeInWithoutAHeldTurnIsStillAFalseAlarm(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	llm := &sequencedLLM{responses: []string{"Sure."}}
	orch := NewWithAllLayers(eouSTT{"Mm.", 0.9, true, nil}, llm, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("one-word"))
	defer stream.Close()
	stream.mu.Lock()
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.state = StateListening
	stream.utteranceSeq = 1
	stream.inflightUtterances = 1
	stream.mu.Unlock()
	stream.processUtterance(make([]byte, 5760), 180*time.Millisecond, 1)
	if n := atomic.LoadInt32(&llm.call); n != 0 {
		t.Fatalf("a one-word barge-in over the agent was answered (%d model calls)", n)
	}
}

// A held turn whose words read as clearly unfinished gets the longer margin; one that only sounds unfinished keeps the
// short wait. Measured the same way as TestEndOfTurnProbabilityDecidesTheWait (confirmWaitMs).
func TestClearlyOpenWordsGetTheLongerWait(t *testing.T) {
	run := func(alt float64, eou ...float64) int64 {
		e := 0.1
		if len(eou) > 0 {
			e = eou[0]
		}
		cfg := DefaultConfig()
		cfg.SilenceTimeout = 0
		cfg.EndOfTurnThreshold = 0.7
		cfg.EndOfTurnWaitMs = 150
		cfg.EndOfTurnOpenThreshold = 0.05
		cfg.EndOfTurnOpenWaitMs = 450
		orch := NewWithAllLayers(eouSTT{"Quería reservar una cita para el.", e, true, &alt}, &sequencedLLM{responses: []string{"Claro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
		stream := orch.NewManagedStream(context.Background(), NewConversationSession("open"))
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
	if w := run(0.001); w < 400 {
		t.Fatalf("clearly open words waited %d ms, want the 450 ms margin", w)
	}
	if w := run(0.5); w < 100 || w > 300 {
		t.Fatalf("words not clearly open waited %d ms, want the 150 ms wait", w)
	}
	if w := run(0.5, 0.01); w < 400 {
		t.Fatalf("sounds clearly open (eou 0.01) waited %d ms, want the 450 ms margin", w)
	}
}
