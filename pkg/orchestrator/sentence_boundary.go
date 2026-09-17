package orchestrator

import (
	"strings"
	"unicode"
)

// Deciding where a spoken sentence actually ends.
//
// The streaming flush used to cut at every '.', '!' and '?'. That is wrong for a surprising amount
// of ordinary speech, and both failure modes are audible:
//
//   - "a las 4 p.m." became three synthesis calls: "a las 4 p.", "m.", and whatever followed.
//     "3.14" became "3." and "14". "un momento..." became "un momento." then two bare dots.
//   - Every fragment pays the engine's start-up cost again (~180 ms on the production node), and
//     the orchestrator serialises TTS, so the fragments queue and the caller hears a GAP where the
//     sentence should have flowed.
//   - Worse, a one- or two-character fragment carries almost no context. The language token is
//     prefixed to it, but there is nothing for the model to apply it to, so "m." comes out as an
//     English letter name in the middle of a Spanish or Catalan line. That is the "it switched to
//     English at the end of a word" report.
//
// So a '.' only ends a sentence when the text around it says it does, and a boundary that would
// produce a fragment too short to be worth synthesising is not a boundary at all.

// minSpokenSegment is the shortest segment worth its own synthesis call, in characters.
//
// Below this the start-up cost dominates the audio produced, and the text is too short to condition
// the model properly — precisely the case that mispronounces. Such a segment waits and goes out
// attached to the next one.
const minSpokenSegment = 12

// abbreviations that end in '.' without ending the sentence. Lower-cased, without the dot.
// Deliberately small: each entry only has to earn its place by appearing in agent speech.
var sentenceAbbreviations = map[string]bool{
	// Spanish / Catalan / Portuguese titles and common short forms
	"sr": true, "sra": true, "srta": true, "sres": true, "dr": true, "dra": true,
	"d": true, "dª": true, "av": true, "avda": true, "c": true, "ctra": true,
	"núm": true, "num": true, "pág": true, "pag": true, "etc": true, "ej": true,
	"aprox": true, "tel": true, "ext": true, "izq": true, "dcha": true,
	// English
	"mr": true, "mrs": true, "ms": true, "prof": true, "st": true, "jr": true,
	"inc": true, "ltd": true, "vs": true, "approx": true, "no": true,
	// French / Italian / German
	"m": true, "mme": true, "mlle": true, "sig": true, "sig.ra": true,
	"hr": true, "fr": true, "nr": true, "bzw": true, "ca": true,
	// Time and unit forms that carry internal dots
	"a": true, "p": true, "am": true, "pm": true,
}

// sentenceEndsAt reports whether the terminator at byte index i in buf ends a sentence.
//
// decided is false when the answer depends on text that has not streamed in yet — the caller must
// then wait rather than flush, because that is exactly how "3." gets spoken before "14" arrives.
// atEOF forces a decision for the final flush, when no more text is coming.
func sentenceEndsAt(buf string, i int, atEOF bool) (isEnd bool, decided bool) {
	if i < 0 || i >= len(buf) {
		return false, atEOF
	}
	switch buf[i] {
	case '!', '?':
		// Rarely ambiguous. Only run-on terminators ("?!", "!!") need care: break after the last
		// one so "¿Sí?!" is a single segment rather than two.
		if i+1 < len(buf) {
			if c := buf[i+1]; c == '!' || c == '?' || c == '.' {
				return false, true // the run continues; a later index is the real end
			}
			return true, true
		}
		return true, atEOF
	case '.':
	default:
		return false, true
	}

	// An ellipsis is a pause inside a sentence, not three sentence ends.
	if i+1 < len(buf) && buf[i+1] == '.' {
		return false, true
	}
	if i > 0 && buf[i-1] == '.' {
		// Last dot of a run. Treat it like any other terminator from here on.
		if i+1 >= len(buf) {
			return false, atEOF
		}
	}

	if i+1 >= len(buf) {
		return false, atEOF // need to see what follows
	}

	next := rune(buf[i+1])
	// "3.14", "1.000" — a decimal or thousands separator.
	if unicode.IsDigit(next) {
		return false, true
	}
	// "google.com", "p.m." — glued to a following letter.
	if unicode.IsLetter(next) {
		return false, true
	}

	// Past this point a space (or other punctuation) follows the dot, which already rules out the
	// glued cases above. The rest of the decision comes from the text BEFORE the dot, which is
	// always available — so a sentence that ends exactly at the edge of an LLM chunk flushes
	// immediately instead of waiting for the next chunk to arrive. Waiting there cost a whole
	// inter-chunk delay on every sentence, which is the gap this work is meant to remove.
	rest := strings.TrimLeft(buf[i+1:], " \t\n\r")
	if rest != "" {
		// When the next word is already in hand, it settles the remaining ambiguity: a sentence
		// starts with a capital, a digit or an opening mark, so a lower-case start means the dot
		// was internal ("etc. y luego").
		if unicode.IsLower([]rune(rest)[0]) {
			return false, true
		}
	}

	// The token immediately before the dot.
	word := buf[:i]
	if sp := strings.LastIndexAny(word, " \t\n\r"); sp >= 0 {
		word = word[sp+1:]
	}
	word = strings.TrimLeft(word, "¿¡\"'«(-—")
	lower := strings.ToLower(word)
	if sentenceAbbreviations[lower] {
		return false, true
	}
	// A bare initial: "J. R. Pérez". One letter before a dot is never a sentence end.
	if r := []rune(word); len(r) == 1 && unicode.IsLetter(r[0]) {
		return false, true
	}

	return true, true
}

// nextFlushPoint returns the exclusive end index of the first segment in buf that can be handed to
// the synthesiser, or -1 if none can yet. clauseBoundaries allows a comma/semicolon/colon to also
// end the first segment of a response (the opening chunk is cut short so the bot starts speaking
// sooner); minLen is the shortest segment worth its own synthesis call.
func nextFlushPoint(buf string, atEOF bool, clauseBoundaries bool, clauseMin int, minLen int) int {
	for i := 0; i < len(buf); i++ {
		switch buf[i] {
		case '.', '!', '?':
			isEnd, decided := sentenceEndsAt(buf, i, atEOF)
			if !decided {
				return -1 // wait for more text rather than guess
			}
			if !isEnd {
				continue
			}
			end := i + 1
			// A boundary that leaves too little to say is not worth taking: the segment would
			// cost a full engine start-up and give the model almost nothing to condition on, so
			// it goes out attached to the next one. This applies at end of stream too — the
			// caller emits whatever remains after the last boundary, so nothing is lost.
			if len([]rune(strings.TrimSpace(buf[:end]))) < minLen {
				continue
			}
			return end
		case ',', ';', ':':
			if clauseBoundaries && i >= clauseMin {
				return i + 1
			}
		}
	}
	return -1
}
