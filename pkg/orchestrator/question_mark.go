package orchestrator

import "strings"

// Restoring question marks the recogniser does not produce.
//
// Parakeet emits punctuation and capitalisation but does not emit '?'. It ends
// questions with a full stop, so "¿me escuchas?" arrives as "Vamos bien, me
// escuchas." and "¿Tienes datos tú mismo de eso?" as "Tienes datos tu mismo de
// eso." The model then reads a statement, answers as though nothing was asked,
// and the caller hears an agent that did not notice it was being questioned.
// Observed on a live Spanish call; the same applies to English.
//
// The fix is lexical rather than prosodic because the prosodic signal is not
// available here: Turno's heads predict turn completion, not interrogativity,
// and a punctuation-restoration model would be another inference pass in the
// hot path for one character of output.
//
// Deliberately conservative. A statement wrongly marked as a question is a
// worse error than a question left unmarked — the model would start answering
// something nobody asked — so this only fires on utterances that OPEN with an
// interrogative, which is where the signal is unambiguous in every language
// handled here. "Dime si puedes" stays a statement; "¿Puedes decirme?" does not.

// interrogativeOpeners are the leading words that make an utterance a question
// almost regardless of what follows. Kept per language rather than merged: "a"
// and "e" style collisions across languages are exactly how a list like this
// starts producing false positives.
var interrogativeOpeners = map[string][]string{
	"en": {
		"what", "why", "when", "where", "who", "whom", "whose", "which", "how",
		"is", "are", "was", "were", "am",
		"do", "does", "did",
		"can", "could", "will", "would", "should", "shall", "may", "might",
		"have", "has", "had",
		"any", "anyone", "anybody",
	},
	"es": {
		"qué", "que", "cómo", "como", "cuándo", "cuando", "dónde", "donde",
		"quién", "quien", "quiénes", "cuál", "cual", "cuáles", "cuánto",
		"cuanto", "cuánta", "cuántos", "cuántas", "por",
		"puedes", "puede", "podrías", "podría", "podemos",
		"tienes", "tiene", "tienen", "hay",
		"eres", "es", "está", "estás", "están", "estoy",
		"me", "sabes", "sabe", "quieres", "quiere", "necesitas",
	},
	"de": {
		"was", "warum", "wann", "wo", "wer", "wen", "wem", "wessen", "welche",
		"welcher", "welches", "wie", "wieso", "weshalb",
		"ist", "sind", "war", "waren", "bin",
		"kann", "kannst", "können", "könnte", "könnten",
		"hast", "hat", "haben", "hatte",
		"darf", "soll", "sollte", "wird", "werden", "würde",
		"gibt",
	},
	"fr": {
		"quoi", "que", "qu'est", "pourquoi", "quand", "où", "qui", "quel",
		"quelle", "quels", "quelles", "comment", "combien",
		"est", "es", "sont", "était",
		"peux", "peut", "pouvez", "pourrais", "pourriez",
		"as", "a", "avez", "avons",
		"y",
	},
	"it": {
		"cosa", "che", "perché", "perche", "quando", "dove", "chi", "quale",
		"quali", "come", "quanto", "quanti",
		"è", "e", "sono", "era",
		"puoi", "può", "potresti", "potrebbe",
		"hai", "ha", "avete",
		"ci",
	},
	"pt": {
		"o", "que", "porquê", "porque", "quando", "onde", "quem", "qual",
		"quais", "como", "quanto", "quantos",
		"é", "são", "era",
		"podes", "pode", "poderia", "podem",
		"tens", "tem", "têm", "há",
	},
}

// restoreQuestionMark returns the transcript with a '?' where the recogniser
// should have put one. Unchanged when the transcript already carries terminal
// punctuation other than a full stop, or when it does not open with an
// interrogative.
func restoreQuestionMark(transcript string, lang Language) string {
	t := strings.TrimSpace(transcript)
	if t == "" {
		return transcript
	}

	// Already marked, or ends in punctuation that asserts something else.
	switch t[len(t)-1] {
	case '?', '!':
		return transcript
	}

	fields := strings.Fields(t)
	if len(fields) < 2 {
		// A single word is as likely to be an answer ("Sí") as a question.
		return transcript
	}

	openers, ok := interrogativeOpeners[normalizeLangKey(lang)]
	if !ok {
		return transcript
	}

	first := strings.ToLower(strings.Trim(fields[0], ".,;:¿¡\"'()"))
	found := false
	for _, w := range openers {
		if first == w {
			found = true
			break
		}
	}
	if !found {
		return transcript
	}

	// Replace a trailing full stop rather than appending after it; leave any
	// other ending alone and just add the mark.
	if t[len(t)-1] == '.' {
		t = strings.TrimRight(t[:len(t)-1], " ")
	}
	return t + "?"
}

// normalizeLangKey maps a Language to the two-letter key used above. Anything
// unrecognised returns "", which disables restoration rather than guessing with
// another language's word list.
func normalizeLangKey(lang Language) string {
	s := strings.ToLower(strings.TrimSpace(string(lang)))
	if s == "" {
		return "en"
	}
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	switch s {
	case "en", "es", "de", "fr", "it", "pt":
		return s
	case "english":
		return "en"
	case "spanish", "castellano":
		return "es"
	case "german", "deutsch":
		return "de"
	case "french", "francais":
		return "fr"
	case "italian", "italiano":
		return "it"
	case "portuguese", "portugues":
		return "pt"
	}
	return ""
}
