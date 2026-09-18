package orchestrator

import (
	"strings"
	"testing"
	"unicode/utf8"
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

// A backchannel is one or two syllables, so an English one dropped into another language is
// unmistakable — this is part of the "it switched to English" report. Catalan, Galician and Basque
// had no case at all and fell through to the English default; French, Italian and Portuguese
// carried a literal "uh-huh".
func TestBackchannelPhrasesAreNotEnglishForOtherLanguages(t *testing.T) {
	english := map[string]bool{"uh-huh": true, "yeah": true, "yep": true, "right": true, "ok": true}
	for _, lang := range []Language{"es", "ca", "gl", "eu", "pt", "fr", "it", "de"} {
		phrases := backchannelPhrasesForLang(lang)
		if len(phrases) == 0 {
			t.Errorf("%s: no backchannel phrases", lang)
			continue
		}
		for _, p := range phrases {
			if english[strings.ToLower(p)] {
				t.Errorf("%s: backchannels with the English %q", lang, p)
			}
		}
	}
}

// Every language the product offers needs its own list, not the English fallback.
func TestEverySupportedLanguageHasItsOwnBackchannels(t *testing.T) {
	fallback := strings.Join(backchannelPhrasesForLang("zz-not-a-language"), "|")
	for _, lang := range []Language{"es", "ca", "gl", "eu", "pt", "fr", "it", "de"} {
		if got := strings.Join(backchannelPhrasesForLang(lang), "|"); got == fallback {
			t.Errorf("%s falls through to the English default (%s)", lang, fallback)
		}
	}
	// English itself is expected to use it.
	if got := strings.Join(backchannelPhrasesForLang("en"), "|"); got != fallback {
		t.Errorf("en = %q, want the default %q", got, fallback)
	}
}

// A long sentence with no terminator used to go to the synthesiser whole, and the synthesiser's
// cost is superlinear in length — so past a few hundred characters it stops keeping ahead of
// playback and the caller hears the reply stop and restart. Measured at nfe 6: RTF 0.203 at 72
// chars, 0.557 at 291, 1.055 at 583. The cap cuts before that point.
func TestLongSentenceIsCutBeforeItOutrunsPlayback(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "140")
	long := "Le he reservado la cita para el martes a las cuatro y media de la tarde en la " +
		"consulta del doctor Martínez que está en la avenida principal número ciento veinte " +
		"y le enviaré un correo de confirmación con todos los detalles"
	if len([]rune(long)) < 200 {
		t.Fatalf("fixture too short to exercise the cap (%d runes)", len([]rune(long)))
	}
	end := nextFlushPoint(long, false, false, 40, minSpokenSegment)
	if end <= 0 {
		t.Fatal("no flush point: a long unterminated sentence would be synthesised whole and gap")
	}
	if n := len([]rune(long[:end])); n > 140 {
		t.Errorf("segment is %d runes, over the 140 cap", n)
	}
	// It must cut where a speaker would pause, not mid-word. Cutting AT a space index leaves the
	// segment ending on the last whole word with no trailing space, so the test is whether the
	// remainder resumes cleanly — not whether the segment ends in one.
	seg, rest := long[:end], long[end:]
	endsClause := strings.HasSuffix(seg, ",") || strings.HasSuffix(seg, ";") || strings.HasSuffix(seg, ":")
	resumesOnWord := strings.HasPrefix(rest, " ")
	if !endsClause && !resumesOnWord {
		t.Errorf("cut mid-word: segment ends %q, remainder starts %q",
			lastRunes(seg, 12), firstRunes(rest, 12))
	}
	if !utf8.ValidString(seg) || !utf8.ValidString(rest) {
		t.Error("cut landed inside a multi-byte character")
	}
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func TestShortTextIsNotCutByTheCap(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "140")
	// Well under the cap and unterminated: the caller must keep buffering, not flush a fragment.
	if got := nextFlushPoint("Le he reservado la cita", false, false, 40, minSpokenSegment); got != -1 {
		t.Errorf("flushed a short unterminated buffer at %d", got)
	}
}

func TestSentenceBoundaryStillWinsOverTheCap(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "140")
	s := "Le he reservado la cita para el martes. Y luego le enviaré un correo de confirmación."
	end := nextFlushPoint(s, false, false, 40, minSpokenSegment)
	if end <= 0 || s[end-1] != '.' {
		t.Errorf("expected the cut at the full stop, got %d (%q)", end, s[:maxInt(0, end)])
	}
}

func TestCapIsConfigurableAndFloored(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "200")
	if got := maxSpokenSegment(); got != 200 {
		t.Errorf("override ignored: got %d", got)
	}
	// A cap below minSpokenSegment would make every segment unflushable.
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "5")
	if got := maxSpokenSegment(); got < 40 {
		t.Errorf("absurd cap accepted: %d", got)
	}
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "")
	if got := maxSpokenSegment(); got != 140 {
		t.Errorf("default = %d, want 140", got)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// splitSentences existed, went through nextFlushPoint (so it inherited the length cap), and was
// called by nobody. Meanwhile the paths that receive a COMPLETE reply — a speculative hit, a
// non-streaming LLM, a cached answer — handed the whole thing to one synthesis call. Since a
// speculative hit is the common case, the common case was the one still exposed to the gap.
func TestSplitSentencesCapsEachSegment(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "140")
	long := "Le he reservado la cita para el martes a las cuatro y media de la tarde en la " +
		"consulta del doctor Martínez que está en la avenida principal número ciento veinte " +
		"y le enviaré un correo de confirmación con todos los detalles y la dirección exacta"
	segs := splitSentences(long)
	if len(segs) < 2 {
		t.Fatalf("a %d-rune reply came back as %d segment(s) — one long synthesis call is the gap",
			len([]rune(long)), len(segs))
	}
	for i, s := range segs {
		if n := len([]rune(s)); n > 140 {
			t.Errorf("segment %d is %d runes, over the cap: %q", i, n, lastRunes(s, 30))
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("segment %d is empty", i)
		}
	}
	// Nothing may be lost or reordered: the words must still read as the original.
	joined := strings.Join(segs, " ")
	norm := func(x string) string { return strings.Join(strings.Fields(x), " ") }
	if norm(joined) != norm(long) {
		t.Errorf("text changed when split:\n got %q\nwant %q", norm(joined), norm(long))
	}
}

func TestSplitSentencesKeepsShortRepliesWhole(t *testing.T) {
	t.Setenv("TTS_MAX_SEGMENT_CHARS", "140")
	// One short sentence must stay one call — splitting it would add a seam for nothing.
	segs := splitSentences("Abre a las nueve.")
	if len(segs) != 1 {
		t.Errorf("short reply split into %d segments: %q", len(segs), segs)
	}
}
