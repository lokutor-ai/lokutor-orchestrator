package orchestrator

import (
	"fmt"
	"strings"
	"testing"
)

// The language instruction is the kind of bug that never throws: the prompt renders, the call
// connects, the agent answers — in the wrong language. It was reported from a live Catalan call
// that opened in Catalan and switched to Spanish the moment the caller spoke.
//
// Two causes, both invisible from the code alone. Catalan, Galician and Basque had no entry in
// languageCodeToName, and its fallback returned the raw code, so the model was instructed to
// "Always respond in ca". And the format string had one more verb than argument slots were
// reasoning about, so the Identity line was fed the language name and read "You are Lokutor's
// voice assistant. Spanish".

func TestEveryShippedLanguageHasARealName(t *testing.T) {
	// The nine Lokutor actually offers; see pkg/api/languages.go in the worker repo. A code that
	// reaches the prompt as a bare code is the Catalan bug returning.
	shipped := map[Language]string{
		LanguageEn: "English", LanguageEs: "Spanish", LanguageCa: "Catalan",
		LanguageGl: "Galician", LanguageEu: "Basque", LanguagePt: "Portuguese",
		LanguageFr: "French", LanguageIt: "Italian", LanguageDe: "German",
	}
	for code, want := range shipped {
		got := languageCodeToName(code)
		if got != want {
			t.Errorf("languageCodeToName(%q) = %q, want %q", code, got, want)
		}
		if got == string(code) {
			t.Errorf("languageCodeToName(%q) returned the bare code — the prompt would read "+
				"\"Always respond in %s\"", code, code)
		}
	}
}

func TestUnknownLanguageStillReadsAsALanguage(t *testing.T) {
	got := languageCodeToName(Language("xx"))
	if got == "xx" {
		t.Fatal("bare code leaked into the prompt for an unmapped language")
	}
	if !strings.Contains(got, "xx") {
		t.Errorf("unknown language should still name the code so the log and prompt agree, got %q", got)
	}
}

func TestSystemPromptNamesTheLanguageEverywhereItPromises(t *testing.T) {
	const persona = "You help people book appointments."
	for _, lang := range []Language{LanguageCa, LanguageEu, LanguageGl, LanguageEs} {
		name := languageCodeToName(lang)
		p := buildSystemPrompt(persona, name)

		if strings.Contains(p, "%!s(MISSING)") || strings.Contains(p, "%!(EXTRA") {
			t.Fatalf("%s: format verbs and arguments disagree:\n%s", lang, p)
		}
		if !strings.Contains(p, persona) {
			t.Errorf("%s: the caller's own prompt was dropped", lang)
		}
		// The Identity line used to be handed the language name, rendering a dangling word.
		if strings.Contains(p, "voice assistant. "+name) {
			t.Errorf("%s: Identity line reads as a dangling language name", lang)
		}
		if !strings.Contains(p, "speaking "+name) {
			t.Errorf("%s: Identity does not say which language it speaks", lang)
		}
		// Three separate instructions depend on the name; a missing one is how drift creeps back.
		if n := strings.Count(p, name); n < 4 {
			t.Errorf("%s: language named only %d times, expected the identity, the instruction, "+
				"the scope and the transcript rule", lang, n)
		}
	}
}

func TestPromptTellsTheModelNotToMirrorAMistranscribedLanguage(t *testing.T) {
	// The recogniser does not cover Catalan, Galician or Basque, so it writes those callers down
	// as Spanish or Portuguese. Without this the model reads the transcript as the user's choice
	// of language and follows it, which is exactly what happened on the reported call.
	p := buildSystemPrompt("persona", "Catalan")
	for _, want := range []string{"recogniser", "NOT the user choosing", "still reply in Catalan"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing the mistranscription rule (%q):\n%s", want, p)
		}
	}
}

func TestSetSystemPromptUsesTheSessionLanguage(t *testing.T) {
	o := &Orchestrator{config: Config{}}
	for _, lang := range []Language{LanguageCa, LanguageGl, LanguageEu} {
		s := &ConversationSession{CurrentLanguage: lang, MaxMessages: 20}
		o.SetSystemPrompt(s, "persona")
		msgs := s.GetContextCopy()
		if len(msgs) == 0 {
			t.Fatalf("%s: no system message was added", lang)
		}
		got := msgs[0].Content
		want := languageCodeToName(lang)
		if !strings.Contains(got, want) {
			t.Errorf("%s: system prompt never names %q", lang, want)
		}
		if strings.Contains(got, fmt.Sprintf("respond in %s.", string(lang))) {
			t.Errorf("%s: prompt instructs with the bare code", lang)
		}
	}
}
