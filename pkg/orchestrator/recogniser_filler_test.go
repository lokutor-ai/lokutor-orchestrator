package orchestrator

import (
	"testing"
	"time"
)

// Two production failures bound this verdict from opposite sides.
//
// 2026-09-19: 440ms of a Spanish caller's opening word came back as "Thank you.", which fired a
// complete turn. The agent answered "De nada.", started speaking it, and the caller's actual
// continuing sentence then registered as a barge-in on that reply.
//
// 2026-09-23: in an English session a spoken "Thank you." (×3), "Yeah." (×3) and "Okay." (×2), each
// 300–640 ms, were every one discarded as noise with no_speech_prob 0. The agent never answered; a
// repeat was dropped exactly like the original.
func TestRecogniserFillerVerdict(t *testing.T) {
	const short = 440 * time.Millisecond
	const long = 1500 * time.Millisecond

	cases := []struct {
		name       string
		transcript string
		lang       Language
		dur        time.Duration
		overAgent  bool
		want       fillerVerdict
	}{
		// 2026-09-23, verbatim: the caller had the floor in an English session.
		{"english thank you with the floor", "Thank you.", LanguageEn, 301 * time.Millisecond, false, fillerTurn},
		{"english okay with the floor", "Okay.", LanguageEn, 413 * time.Millisecond, false, fillerTurn},
		{"english yeah with the floor", "Yeah.", LanguageEn, 640 * time.Millisecond, false, fillerTurn},
		{"auto-detect behaves like english", "Okay.", "", short, false, fillerTurn},

		// Over the agent the reply resumes, and a short "yeah" is a backchannel.
		{"english yeah over the agent", "Yeah.", LanguageEn, short, true, fillerDiscard},
		{"spanish yeah over the agent", "Yeah.", "es", long, true, fillerDiscard},

		// 2026-09-19, verbatim: an English phrase in a Spanish call is not what was said.
		{"english filler in a spanish call", "Thank you.", "es", short, false, fillerDiscard},
		{"case and punctuation are not a defence", "THANK YOU!!", "es", short, false, fillerDiscard},
		{"catalan call", "thanks", "ca", short, false, fillerDiscard},
		// ...but a second or more of it is real speech the recogniser could not read.
		{"long filler in a spanish call asks", "Thank you.", "es", long, false, fillerAskRepeat},

		// Hesitations are never a turn; caption boilerplate needs time to have been said.
		{"hesitation with the floor", "Um.", LanguageEn, short, false, fillerDiscard},
		{"caption boilerplate, implausibly short", "Thanks for watching!", LanguageEn, short, false, fillerDiscard},
		{"caption boilerplate, plausible duration", "Thanks for watching!", LanguageEn, long, false, fillerTurn},

		// Real speech is never a filler, however short.
		{"real spanish utterance", "Abre la oficina mañana.", "es", short, false, fillerNone},
		{"short real answer", "sí", "es", short, true, fillerNone},
		{"filler as a substring of real speech", "thank you for opening the office", LanguageEn, long, false, fillerNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recogniserFillerVerdict(tc.transcript, tc.lang, tc.dur, tc.overAgent); got != tc.want {
				t.Errorf("recogniserFillerVerdict(%q, %q, %v, overAgent=%v) = %v, want %v",
					tc.transcript, tc.lang, tc.dur, tc.overAgent, got, tc.want)
			}
		})
	}
}

// isLikelyNoise stops a turn outright, so it must keep to signals that hold in every context, and
// must not panic on a stream without a session.
func TestIsLikelyNoiseKeepsRealSpeech(t *testing.T) {
	ms := &ManagedStream{session: NewConversationSession("test")}
	ms.session.CurrentLanguage = "es"

	if ms.isLikelyNoise(TranscriptionResult{Text: "Abre la oficina mañana."}, 2*time.Second) {
		t.Error("real speech was discarded as noise")
	}
	if ms.isLikelyNoise(TranscriptionResult{Text: "Thank you."}, 440*time.Millisecond) {
		t.Error("isLikelyNoise judged a filler phrase; that needs to know who had the floor")
	}
	if !ms.isLikelyNoise(TranscriptionResult{Text: ""}, 440*time.Millisecond) {
		t.Error("an empty transcript was not noise")
	}

	noSession := &ManagedStream{}
	if noSession.isLikelyNoise(TranscriptionResult{Text: "hola"}, time.Second) {
		t.Error("real speech discarded when no session is attached")
	}
}
