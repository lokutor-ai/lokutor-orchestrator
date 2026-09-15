package orchestrator

import "testing"

// The real transcripts that exposed this, from a live Spanish call. Parakeet
// ended every one of them with a full stop.
func TestRestoresQuestionMarkOnObservedTranscripts(t *testing.T) {
	cases := []struct {
		lang Language
		in   string
		want string
	}{
		{"es", "Tienes datos tu mismo de eso.", "Tienes datos tu mismo de eso?"},
		{"es", "Me escuchas cuando hablo", "Me escuchas cuando hablo?"},
		{"es", "Puedes explicar una historia", "Puedes explicar una historia?"},
		{"es", "Qué hora es.", "Qué hora es?"},
		{"en", "Do you work.", "Do you work?"},
		{"en", "Can you help me with this", "Can you help me with this?"},
		{"en", "What are your opening hours.", "What are your opening hours?"},
		{"de", "Kannst du mir helfen.", "Kannst du mir helfen?"},
	}
	for _, c := range cases {
		if got := restoreQuestionMark(c.in, c.lang); got != c.want {
			t.Errorf("%s %q -> %q, want %q", c.lang, c.in, got, c.want)
		}
	}
}

// A statement turned into a question is the worse error: the model starts
// answering something nobody asked. These must all be left alone.
func TestLeavesStatementsAlone(t *testing.T) {
	cases := []struct {
		lang Language
		in   string
	}{
		{"es", "Vamos bien."},
		{"es", "Dime si puedes ayudarme."},
		{"es", "Necesito ayuda con un ticket."},
		{"es", "Bueno, pues no me va la página."},
		{"en", "I need help with a ticket."},
		{"en", "Tell me if you can help."},
		{"en", "Thanks, that works."},
		{"de", "Ich brauche Hilfe."},
	}
	for _, c := range cases {
		if got := restoreQuestionMark(c.in, c.lang); got != c.in {
			t.Errorf("%s %q was rewritten to %q — statements must not become questions", c.lang, c.in, got)
		}
	}
}

func TestLeavesExistingTerminalPunctuationAlone(t *testing.T) {
	for _, in := range []string{
		"Do you work?",
		"¿Me escuchas?",
		"What!",
	} {
		if got := restoreQuestionMark(in, "en"); got != in {
			t.Errorf("%q -> %q, want unchanged", in, got)
		}
	}
}

// A single word is as likely to be an answer as a question.
func TestSingleWordIsNotAQuestion(t *testing.T) {
	for _, in := range []string{"Sí", "Yes", "What"} {
		if got := restoreQuestionMark(in, "en"); got != in {
			t.Errorf("%q -> %q, want unchanged", in, got)
		}
	}
}

// An unknown language must disable restoration rather than fall back to another
// language's word list, which would fire on unrelated words.
func TestUnknownLanguageDoesNothing(t *testing.T) {
	in := "Kas sa kuuled mind"
	if got := restoreQuestionMark(in, "et"); got != in {
		t.Errorf("%q -> %q, want unchanged for an unsupported language", in, got)
	}
}

func TestLanguageKeyNormalisation(t *testing.T) {
	for _, c := range []struct {
		in   Language
		want string
	}{
		{"es", "es"}, {"es-ES", "es"}, {"Spanish", "es"},
		{"en_US", "en"}, {"", "en"}, {"klingon", ""},
	} {
		if got := normalizeLangKey(c.in); got != c.want {
			t.Errorf("normalizeLangKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmptyTranscriptUnchanged(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if got := restoreQuestionMark(in, "es"); got != in {
			t.Errorf("%q -> %q", in, got)
		}
	}
}
