package orchestrator

import (
	"strings"
	"testing"
)

// Splitting at every '.' turned ordinary speech into a stream of fragments. Each one paid the
// engine's start-up cost again — heard as a gap mid-sentence — and a one- or two-character
// fragment gives the model almost nothing for the language token to act on, so it came out
// English-sounding inside a Spanish or Catalan line.
func TestSplitSentencesKeepsInternalDotsTogether(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{"decimal", "El total es 3.14 euros.", []string{"El total es 3.14 euros."}},
		{"thousands", "Son 1.500 personas en total.", []string{"Son 1.500 personas en total."}},
		{"time abbreviation", "Le espero a las 4 p.m. en la clinica.",
			[]string{"Le espero a las 4 p.m. en la clinica."}},
		{"title", "Le atendera el Dr. Garcia manana por la tarde.",
			[]string{"Le atendera el Dr. Garcia manana por la tarde."}},
		{"ellipsis", "Un momento... ahora mismo lo compruebo.",
			[]string{"Un momento... ahora mismo lo compruebo."}},
		{"domain", "Escribanos a hola.lokutor.com cuando quiera.",
			[]string{"Escribanos a hola.lokutor.com cuando quiera."}},
		{"initials", "Le atiende J. R. Martinez esta tarde.",
			[]string{"Le atiende J. R. Martinez esta tarde."}},
		{"etcetera", "Traiga la cartilla, el DNI, etc. y le atendemos enseguida.",
			[]string{"Traiga la cartilla, el DNI, etc. y le atendemos enseguida."}},

		// Real sentence ends still split — the point is not to stop splitting.
		{"two sentences", "Buenos dias y bienvenido. En que puedo ayudarle hoy?",
			[]string{"Buenos dias y bienvenido.", "En que puedo ayudarle hoy?"}},
		{"question then statement", "Le viene bien el martes? Se lo confirmo ahora mismo.",
			[]string{"Le viene bien el martes?", "Se lo confirmo ahora mismo."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := splitSentences(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("split into %d segments %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("segment %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// Whatever the split, every word must survive it intact and in order — a word broken across two
// synthesis calls is the failure that sounded like "conversación" becoming "conver" + "Asian".
func TestSplitSentencesNeverBreaksAWord(t *testing.T) {
	for _, in := range []string{
		"Vamos a tener una conversacion muy interesante sobre el tema.",
		"El total es 3.14 euros. Le espero a las 4 p.m. en la clinica del Dr. Garcia.",
		"Un momento... le confirmo la cita. Gracias por su paciencia!",
		"Hola.",
		"Bon dia, avui parlarem de la conversacio en catala. Li va be?",
	} {
		segs := splitSentences(in)
		rejoined := strings.Join(segs, " ")
		wantWords := strings.Fields(in)
		gotWords := strings.Fields(rejoined)
		if len(gotWords) != len(wantWords) {
			t.Fatalf("%q -> %q: %d words, want %d (a word was split or lost)",
				in, segs, len(gotWords), len(wantWords))
		}
		for i := range wantWords {
			if gotWords[i] != wantWords[i] {
				t.Errorf("%q: word %d = %q, want %q", in, i, gotWords[i], wantWords[i])
			}
		}
	}
}

// A segment too short to be worth synthesising costs a full engine start-up and gives the language
// token nothing to condition, so it rides along with the next one instead.
func TestSplitSentencesDoesNotEmitTinyFragments(t *testing.T) {
	for _, in := range []string{
		"Si. Claro que si, ahora mismo se lo confirmo.",
		"Vale. Perfecto. Le he reservado la cita para el martes.",
		"Ya. Entiendo.",
	} {
		for _, seg := range splitSentences(in) {
			if n := len([]rune(seg)); n < minSpokenSegment && seg != in {
				t.Errorf("%q produced a %d-character segment %q", in, n, seg)
			}
		}
	}
}

// The streaming flush must not guess ahead of the text: deciding "3." is a sentence before "14"
// arrives is exactly how a number got read as two.
func TestNextFlushPointWaitsWhenTheNextCharacterDecides(t *testing.T) {
	// "3." could be a sentence end or a decimal — undecidable until the next character.
	if got := nextFlushPoint("El total es 3.", false, false, 0, minSpokenSegment); got != -1 {
		t.Errorf("flushed at %d without seeing what follows the dot", got)
	}
	// With the digit in hand it is clearly a decimal, so still no flush.
	if got := nextFlushPoint("El total es 3.14 euros", false, false, 0, minSpokenSegment); got != -1 {
		t.Errorf("flushed at %d inside a decimal", got)
	}
	// At end of stream there is simply no boundary inside "El total es 3." — the caller emits
	// the remainder, which is how the text survives. What must not happen is a split.
	if got := nextFlushPoint("El total es 3.", true, false, 0, minSpokenSegment); got != -1 {
		t.Errorf("split at %d inside %q at end of stream", got, "El total es 3.")
	}
	if segs := splitSentences("El total es 3."); len(segs) != 1 || segs[0] != "El total es 3." {
		t.Errorf("splitSentences lost or split the text: %q", segs)
	}
}

// A sentence that ends exactly at the edge of an LLM chunk has to flush immediately. Waiting for
// the next chunk to confirm cost a whole inter-chunk delay on every sentence.
func TestNextFlushPointDoesNotWaitOnATrailingSpace(t *testing.T) {
	got := nextFlushPoint("First sentence. ", false, false, 0, minSpokenSegment)
	if got != len("First sentence.") {
		t.Errorf("flush point %d, want %d — a completed sentence must not wait for the next chunk",
			got, len("First sentence."))
	}
}

func TestNextFlushPointStillTakesClauseBoundariesForTheOpeningChunk(t *testing.T) {
	buf := "Buenos dias y bienvenido, en que puedo ayudarle"
	got := nextFlushPoint(buf, false, true, 12, minSpokenSegment)
	if got != strings.IndexByte(buf, ',')+1 {
		t.Errorf("flush point %d, want the comma at %d", got, strings.IndexByte(buf, ',')+1)
	}
	// ...but only when the opening-chunk split is enabled.
	if got := nextFlushPoint(buf, false, false, 12, minSpokenSegment); got != -1 {
		t.Errorf("flushed at a comma (%d) with clause boundaries disabled", got)
	}
}
