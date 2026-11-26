package orchestrator

import (
	"strings"
	"testing"
)

// A call's speaking rules carry examples in its own language when the platform has them: a Catalan call that read Spanish
// examples said "uf, vaya" and "a ver" to Catalan callers (2026-11-26).
func TestSpeakingRulesAreInTheCallsLanguage(t *testing.T) {
	spanishOnly := []string{"uf, vaya", "a ver", "¿no?", "es que hoy no abrimos", "¿Hay algo más"}
	englishOnly := []string{"I, I mean", "Great question!", "oh, nice"}
	catalan := buildSystemPrompt("persona", "Catalan")
	for _, w := range append(spanishOnly, englishOnly...) {
		if strings.Contains(catalan, w) {
			t.Errorf("Catalan call: %q from another language's examples", w)
		}
	}
	for _, w := range []string{"a veure", "d'acord", "Hi ha res més en què el pugui ajudar?"} {
		if !strings.Contains(catalan, w) {
			t.Errorf("Catalan call: missing %q", w)
		}
	}
	// "em" is a Catalan word ("em dius el teu nom?"), not a hesitation sound there.
	if strings.Contains(catalan, `"em",`) {
		t.Error(`Catalan call: "em" listed as a sound`)
	}
	english := buildSystemPrompt("persona", "English")
	for _, w := range spanishOnly {
		if strings.Contains(english, w) {
			t.Errorf("English call: %q", w)
		}
	}
	spanish := buildSystemPrompt("persona", "Spanish")
	for _, w := range englishOnly {
		if strings.Contains(spanish, w) {
			t.Errorf("Spanish call: %q", w)
		}
	}
	// Every other language, and a call with no language pinned, keeps the Spanish and English examples.
	for _, p := range []string{buildSystemPrompt("persona", "French"), buildSystemPromptAutoLanguage("persona")} {
		if !strings.Contains(p, "el jueves lo tengo fatal") || !strings.Contains(p, "I could ask, though?") {
			t.Errorf("no examples:\n%s", p)
		}
	}
}

// The rules every language shares (2026-11-26): what is written is spoken, and a self-correction is never of a fact.
func TestSpeakingRulesEveryLanguageShares(t *testing.T) {
	for _, lang := range []string{"Spanish", "Catalan", "English", "French", ""} {
		s := speakingSection(lang)
		for _, want := range []string{
			"every word you write is said to the caller as it stands",
			"One question at most",
			"never a fact",
			"Don't read back what they said; confirm only what matters.",
			"Numbers, dates and times as words, never digits.",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%q: missing %q", lang, want)
			}
		}
		if strings.Contains(s, "el martes, bueno, no, el miércoles") {
			t.Errorf("%q: the self-correction example corrects a fact", lang)
		}
		if !strings.HasPrefix(s, "# How you speak\n") || strings.Contains(s, "%") {
			t.Errorf("%q: section header or a format verb:\n%s", lang, s)
		}
	}
}
