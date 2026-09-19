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

// The startup order is SetSystemPrompt then SetLanguage, and the session starts on its LanguageEn
// default — so the prompt is always BORN English and the language change has to correct it.
//
// It used to correct it with a regex for "Always respond in X.", which is one of the nine places
// the language section names the language. The other eight kept saying English, including "still
// reply in English" and "every word you produce must be in English". A Spanish call therefore ran
// on a prompt arguing 8-to-1 for English, and the model drifted into English mid-call — reported
// from real calls twice.
// instructionsNaming returns the phrasings that actually command a language, as opposed to merely
// mentioning it. "a short English-looking fragment" mentions English and is correct in a Spanish
// prompt; "Answer it in English" commands it and is the bug.
func instructionsNaming(name string) []string {
	return []string{
		"Always respond in " + name,
		"must be in " + name,
		"still reply in " + name,
		"Answer it in " + name,
		"repeat — in " + name,
		"A sentence in " + name,
		"failing on " + name + " audio",
		"speaking " + name,
	}
}

var englishInstructions = instructionsNaming("English")

func TestSetLanguageLeavesNoTraceOfThePreviousLanguage(t *testing.T) {
	o := &Orchestrator{config: Config{}}
	for _, lang := range []Language{LanguageEs, LanguageCa, LanguageGl, LanguageEu, LanguageFr} {
		s := NewConversationSession("s") // starts on LanguageEn, as production does
		o.SetSystemPrompt(s, "You help people book appointments.")
		o.SetLanguage(s, lang)

		got := s.GetContextCopy()[0].Content
		want := languageCodeToName(lang)

		// The only acceptable number of English INSTRUCTIONS in a Spanish call is zero. The word
		// itself is allowed to survive: "a short English-looking fragment" describes the
		// recogniser's failure mode and is correct in every language.
		for _, stale := range englishInstructions {
			if strings.Contains(got, stale) {
				t.Errorf("%s: prompt still says %q — the model sees two languages at once:\n%s",
					lang, stale, got)
			}
		}
		if n := strings.Count(got, want); n < 4 {
			t.Errorf("%s: after SetLanguage the prompt names %q only %d times; the section that "+
				"holds the model on one language was not rebuilt", lang, want, n)
		}
		if !strings.Contains(got, "still reply in "+want) {
			t.Errorf("%s: the mistranscription rule still names the old language", lang)
		}
		if !strings.Contains(got, "You help people book appointments.") {
			t.Errorf("%s: rebuilding the prompt dropped the agent's own text", lang)
		}
	}
}

// Switching language twice must not leave sediment from the first switch either.
func TestSetLanguageIsIdempotentAcrossSwitches(t *testing.T) {
	o := &Orchestrator{config: Config{}}
	s := NewConversationSession("s")
	o.SetSystemPrompt(s, "persona")
	o.SetLanguage(s, LanguageEs)
	o.SetLanguage(s, LanguageCa)
	o.SetLanguage(s, LanguageGl)

	got := s.GetContextCopy()[0].Content
	for _, staleLang := range []string{"English", "Spanish", "Catalan"} {
		for _, stale := range instructionsNaming(staleLang) {
			if strings.Contains(got, stale) {
				t.Errorf("prompt still says %q after switching to Galician:\n%s", stale, got)
			}
		}
	}
	if !strings.Contains(got, "Galician") {
		t.Error("prompt does not name the current language")
	}
}

// An unset language means "follow the caller", not "speak English". languageCodeToName mapped ""
// to "English", so an agent with no language configured got the full nine-mention English section
// — the strongest instruction in the prompt, telling it to override the caller it was supposed to
// follow.
func TestUnpinnedLanguageDoesNotInstructEnglish(t *testing.T) {
	o := &Orchestrator{config: Config{}}
	for _, lang := range []Language{"", "auto", "na"} {
		if languageIsPinned(lang) {
			t.Errorf("languageIsPinned(%q) = true; auto-detect would be treated as a real language", lang)
		}
		s := &ConversationSession{CurrentLanguage: lang, MaxMessages: 20}
		o.SetSystemPrompt(s, "persona")
		got := s.GetContextCopy()[0].Content

		if strings.Contains(got, "Always respond in English") {
			t.Errorf("%q: unconfigured language renders as an instruction to speak English:\n%s", lang, got)
		}
		if !strings.Contains(got, "use the one the caller speaks") {
			t.Errorf("%q: prompt does not tell the model to follow the caller", lang)
		}
		// Following the caller is not enough on its own: the recogniser emits English-looking
		// fragments, and a model told only to mirror them switches to English on the first "Yeah".
		if !strings.Contains(got, "recogniser") || !strings.Contains(got, "stay in it") {
			t.Errorf("%q: auto-detect prompt lacks the stay-put rule that stops fragment-driven "+
				"drift:\n%s", lang, got)
		}
		if !strings.Contains(got, "persona") {
			t.Errorf("%q: the agent's own prompt was dropped", lang)
		}
	}
}
