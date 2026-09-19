package orchestrator

import (
	"strings"
	"testing"
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
