package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// When the recogniser returns nothing for real speech, the caller must be told — in their own
// language.
//
// From a live Spanish call: 2459 ms of speech came back as "" and the agent said nothing at all.
// The caller had to speak a second time to get any response, with no way to know whether to repeat
// themselves or hang up. Silence is the correct answer to a cough; it is the worst possible answer
// to a sentence.
func TestNotHeardPromptSpeaksTheCallersLanguage(t *testing.T) {
	for _, tc := range []struct {
		lang Language
		want string
	}{
		{"es", "Perdona"},
		{"ca", "Perdona"},
		{"gl", "Perdoa"},
		{"eu", "Barkatu"},
		{"pt", "Desculpa"},
		{"it", "Scusa"},
		{"fr", "Pardon"},
		{"de", "Entschuldigung"},
		{LanguageEn, "Sorry"},
		{"", "Sorry"}, // auto-detect falls back to English rather than guessing
	} {
		got := notHeardPrompt(tc.lang)
		if got == "" {
			t.Errorf("lang %q produced no prompt at all", tc.lang)
			continue
		}
		if len(got) > 60 {
			t.Errorf("lang %q prompt is %d chars; this interrupts a caller and must stay short: %q",
				tc.lang, len(got), got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("lang %q = %q, expected it to be in that language (looked for %q)",
				tc.lang, got, tc.want)
		}
	}
}

// The English fallback must never be handed to a non-English caller: this fires exactly when the
// recogniser is struggling, and its own failure mode on Spanish audio is to emit English. Mirroring
// that back is the one thing not to do.
func TestNotHeardPromptNeverFallsBackToEnglishForAKnownLanguage(t *testing.T) {
	en := notHeardPrompt(LanguageEn)
	for _, lang := range []Language{"es", "ca", "gl", "eu", "pt", "it", "fr", "de"} {
		if notHeardPrompt(lang) == en {
			t.Errorf("lang %q fell back to the English prompt", lang)
		}
	}
}

// nsSTT answers every utterance with an empty transcript and the recogniser's no-speech probability.
type nsSTT struct{ noSpeech float64 }

func (s nsSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	return TranscriptionResult{NoSpeechProb: s.noSpeech}, nil
}
func (s nsSTT) Name() string { return "ns" }

// A caller's whole "How does Lokutor compare to ElevenLabs?", spoken over the opening (2.16 s), came back from the
// recogniser as "" (2026-10-22, live). isLikelyNoise calls every empty transcript noise, so the branch that asks a
// caller to repeat after a long empty transcript never ran: "Utterance discarded as noise, resuming bot", with the
// opening already stopped and nothing to resume. It runs now, unless the recogniser says no speech, the utterance is
// short, or a false alarm puts the caller back in a reply that is still going.
func TestLongEmptyTranscriptAsksTheCallerToRepeat(t *testing.T) {
	const asked = "WARN Empty transcript for a long utterance"
	for _, c := range []struct {
		name     string
		noSpeech float64
		dur      time.Duration
		resumes  bool
		wantAsk  bool
	}{
		{"over a reply already stopped", 0, 2160 * time.Millisecond, false, true},
		{"over a reply still being synthesised: it resumes", 0, 2160 * time.Millisecond, true, false},
		{"short: a cough, a door", 0, 600 * time.Millisecond, false, false},
		{"the recogniser says no speech", 0.9, 2160 * time.Millisecond, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &recordingLogger{}
			cfg := DefaultConfig()
			cfg.SilenceTimeout = 0
			orch := NewWithAllLayers(nsSTT{c.noSpeech}, &sequencedLLM{responses: []string{"Hello."}}, fixedAudioTTS{}, nil, cfg, log)
			stream := orch.NewManagedStream(context.Background(), NewConversationSession("empty"))
			defer stream.Close()
			stream.SetTransportPlaysThroughBargeIn(true)
			stream.mu.Lock()
			stream.pendingBargeIn = true
			stream.pendingBargeGen = stream.payloadGen
			stream.onset = heardSnapshot{gen: stream.payloadGen, at: time.Now().Add(-2 * time.Second), stopped: true}
			if c.resumes {
				stream.ttsCancel = func() {}
			}
			stream.state = StateListening
			stream.utteranceSeq = 1
			stream.inflightUtterances = 1
			stream.mu.Unlock()
			stream.processUtterance(make([]byte, int(c.dur.Milliseconds())*32), c.dur, 1)
			if got := log.count(asked) > 0; got != c.wantAsk {
				t.Fatalf("asked the caller to repeat: %v, want %v (log: %v)", got, c.wantAsk, log.lines)
			}
		})
	}
}
