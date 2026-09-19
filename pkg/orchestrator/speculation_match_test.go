package orchestrator

import "testing"

// Whether a speculative answer is reusable is decided by whether the same WORDS were said, not by
// whether two transcripts are byte-identical.
//
// The comparison was strings.EqualFold on trimmed strings, and the exactness was quietly expensive:
// the speculative transcript comes from the hangover window and the final one from the completed
// utterance, and the recogniser renders the same words with different punctuation and
// capitalisation all the time. A finished, correct answer was discarded and the whole LLM call paid
// again. Measured in production: speculation SEEDED on 16 turns, USED on 1. That gap is the
// difference between the VAD hangover being free and it costing 239ms on the critical path, which
// is the entire reason for speculating.
func TestSpeculativeMatchIgnoresOnlyWhatCannotChangeTheAnswer(t *testing.T) {
	same := []struct{ a, b, why string }{
		{"abre la oficina mañana", "Abre la oficina mañana.", "capitalisation and a trailing period"},
		{"¿Sí?", "sí", "opening and closing punctuation, and case"},
		{"  hola   qué tal  ", "hola qué tal", "leading, trailing and repeated whitespace"},
		{"Vale, gracias!", "vale gracias", "internal comma and an exclamation"},
	}
	for _, c := range same {
		if !sameUtterance(c.a, c.b) {
			t.Errorf("should match (%s): %q vs %q", c.why, c.a, c.b)
		}
	}

	// The other half, and the more important one: a speculative answer to a DIFFERENT question is
	// the wrong answer, and delivering it fast is worse than delivering the right one late.
	different := []struct{ a, b, why string }{
		{"abre la oficina mañana", "cierra la oficina mañana", "a different verb changes the request"},
		{"abre la oficina mañana", "abre la oficina el lunes", "a different day"},
		{"abre la oficina", "abre la oficina mañana", "the final transcript carries a word the guess did not"},
		{"sí", "no", "the shortest possible reversal"},
		{"sí", "si", "accented and unaccented are different Spanish words (yes vs if)"},
		{"", "hola", "an empty guess must never match anything"},
		{"hola", "", "nor be matched by an empty final"},
	}
	for _, c := range different {
		if sameUtterance(c.a, c.b) {
			t.Errorf("must NOT match (%s): %q vs %q", c.why, c.a, c.b)
		}
	}
}

// Accents are preserved deliberately: in Spanish they distinguish real words, so folding them would
// let a speculative answer to one question be served for another.
func TestSpeculativeMatchKeepsAccentsSignificant(t *testing.T) {
	if sameUtterance("papa", "papá") {
		t.Error(`"papa" and "papá" are different words and must not match`)
	}
	if sameUtterance("el", "él") {
		t.Error(`"el" and "él" are different words and must not match`)
	}
}
