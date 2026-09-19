package orchestrator

import (
	"testing"
	"time"
)

// The production failure this guards, end to end: 440ms of a Spanish caller's
// opening word came back as "Thank you.", which fired a complete turn. The agent
// answered "De nada.", started speaking it, and the caller's actual continuing
// sentence then registered as a barge-in on that reply — so the caller heard
// nothing and had answered nothing, while every log line looked healthy.
func TestRecogniserFillerRejection(t *testing.T) {
	const short = 440 * time.Millisecond
	const long = 3 * time.Second

	cases := []struct {
		name       string
		transcript string
		lang       Language
		dur        time.Duration
		want       bool
	}{
		// The observed case.
		{"english filler in a spanish call", "Thank you.", "es", short, true},
		{"same filler, longer audio, still spanish", "Thank you.", "es", long, true},
		{"case and punctuation are not a defence", "THANK YOU!!", "es", short, true},
		{"catalan call", "thanks", "ca", short, true},

		// English calls: the phrase is genuinely sayable, so it needs the
		// corroborating signal of implausibly short audio.
		{"english call, implausibly short", "Thank you.", LanguageEn, short, true},
		{"english call, plausible duration", "Thank you.", LanguageEn, long, false},

		// Auto-detect is not "not English" — the caller may be speaking it.
		{"auto-detect behaves like english", "Thank you.", "", long, false},
		{"auto-detect, implausibly short", "Thank you.", "", short, true},

		// Real speech must never be caught, however short.
		{"real spanish utterance", "Abre la oficina mañana.", "es", short, false},
		{"short real answer", "sí", "es", short, false},
		{"filler as a substring of real speech", "thank you for opening the office", LanguageEn, long, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRecogniserFiller(tc.transcript, tc.lang, tc.dur); got != tc.want {
				t.Errorf("isRecogniserFiller(%q, %q, %v) = %v, want %v",
					tc.transcript, tc.lang, tc.dur, got, tc.want)
			}
		})
	}
}

// isLikelyNoise is the gate that actually stops the turn, so the wiring matters as
// much as the predicate — and it must not panic on a stream without a session.
func TestIsLikelyNoiseRejectsFillerAndKeepsRealSpeech(t *testing.T) {
	ms := &ManagedStream{session: NewConversationSession("test")}
	ms.session.CurrentLanguage = "es"

	if !ms.isLikelyNoise(TranscriptionResult{Text: "Thank you."}, 440*time.Millisecond) {
		t.Error("a hallucinated English filler in a Spanish call reached the LLM")
	}
	if ms.isLikelyNoise(TranscriptionResult{Text: "Abre la oficina mañana."}, 2*time.Second) {
		t.Error("real speech was discarded as noise")
	}

	noSession := &ManagedStream{}
	if noSession.isLikelyNoise(TranscriptionResult{Text: "hola"}, time.Second) {
		t.Error("real speech discarded when no session is attached")
	}
}
