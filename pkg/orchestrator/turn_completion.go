package orchestrator

import (
	"regexp"
	"strings"
)

type TurnCompletionAnalyzer struct {
	incompletePatterns []*regexp.Regexp
	completionMarkers  []*regexp.Regexp
}

func NewTurnCompletionAnalyzer() *TurnCompletionAnalyzer {
	return &TurnCompletionAnalyzer{
		incompletePatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\band\s*$`),
			regexp.MustCompile(`(?i)\bor\s*$`),
			regexp.MustCompile(`(?i)\bbut\s*$`),
			regexp.MustCompile(`(?i)\bbecause\s*$`),
			regexp.MustCompile(`(?i)\blike\s*$`),
			regexp.MustCompile(`(?i)\byou\s+know\s*$`),
			regexp.MustCompile(`(?i)\bI\s+mean\s*$`),
			regexp.MustCompile(`(?i),\s*$`),
			regexp.MustCompile(`(?i)\.\.\.\s*$`),
			regexp.MustCompile(`(?i)\bwhich\s*$`),
			regexp.MustCompile(`(?i)\bthat\s*$`),
			regexp.MustCompile(`(?i)\bwhen\s*$`),
			regexp.MustCompile(`(?i)\bif\s*$`),
			regexp.MustCompile(`(?i)\bso\s*$`),

			regexp.MustCompile(`(?i)\by\s*$`),
			regexp.MustCompile(`(?i)\bo\s*$`),
			regexp.MustCompile(`(?i)\bpero\s*$`),
			regexp.MustCompile(`(?i)\bporque\s*$`),
			regexp.MustCompile(`(?i)\bcuando\s*$`),
			regexp.MustCompile(`(?i)\bcomo\s*$`),
			regexp.MustCompile(`(?i)\bdonde\s*$`),
			regexp.MustCompile(`(?i)\bque\s*$`),
			regexp.MustCompile(`(?i)\bsi\s*$`),
			regexp.MustCompile(`(?i)\bentonces\s*$`),
			regexp.MustCompile(`(?i)\bes\s+decir\s*$`),
			regexp.MustCompile(`(?i)\bo\s+sea\s*$`),
			regexp.MustCompile(`(?i)\bes\s+que\s*$`),
			regexp.MustCompile(`(?i)\bpues\s*$`),

			regexp.MustCompile(`(?i)\bet\s*$`),
			regexp.MustCompile(`(?i)\bou\s*$`),
			regexp.MustCompile(`(?i)\bmais\s*$`),
			regexp.MustCompile(`(?i)\bporque\s*$`),
			regexp.MustCompile(`(?i)\bquando\s*$`),
		},
		completionMarkers: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\?\s*$`),
			regexp.MustCompile(`(?i)!\s*$`),
			regexp.MustCompile(`(?i)\.\s*$`),
			regexp.MustCompile(`(?i)right\?\s*$`),
			regexp.MustCompile(`(?i)yeah\s*$`),
			regexp.MustCompile(`(?i)okay\s*$`),
			regexp.MustCompile(`(?i)sure\s*$`),

			regexp.MustCompile(`(?i)vale\s*$`),
			regexp.MustCompile(`(?i)sí\s*$`),
			regexp.MustCompile(`(?i)claro\s*$`),
			regexp.MustCompile(`(?i)ok\s*$`),
		},
	}
}

func (tca *TurnCompletionAnalyzer) IsLikelyComplete(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	// Trailing ellipsis ALWAYS means mid-thought. It must be checked before
	// completionMarkers because the generic `\.\s*$` marker would otherwise
	// match the final dot of "..." and misclassify the utterance as complete.
	if regexp.MustCompile(`\.{3,}\s*$`).MatchString(text) {
		return false
	}

	for _, p := range tca.completionMarkers {
		if p.MatchString(text) {
			return true
		}
	}

	for _, p := range tca.incompletePatterns {
		if p.MatchString(text) {
			return false
		}
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}

	lastWord := strings.ToLower(words[len(words)-1])

	if len(words) <= 2 {
		affirms := map[string]bool{
			"yes": true, "yeah": true, "okay": true, "ok": true,
			"sure": true, "nope": true, "no": true, "sí": true,
			"vale": true, "claro": true, "nah": true,
		}
		if affirms[strings.Trim(lastWord, ".,!?")] {
			return true
		}
		return false
	}

	incomplete := map[string]bool{
		"a": true, "an": true, "the": true,
		"i": true, "you": true, "he": true, "she": true, "we": true, "they": true,
		"this": true, "that": true, "these": true, "those": true,
		"is": true, "are": true, "was": true, "were": true,
		"would": true, "could": true, "should": true, "might": true,
		"el": true, "la": true, "los": true, "las": true, "un": true, "una": true,
		"este": true, "esta": true, "eso": true, "esa": true,
	}
	clean := strings.Trim(lastWord, ".,!?")
	if incomplete[clean] {
		return false
	}

	return true
}

func (tca *TurnCompletionAnalyzer) CombinedCompletionScore(text string, durationMs int) float64 {
	semantic := 0.5
	if tca.IsLikelyComplete(text) {
		semantic = 0.7
	} else {
		semantic = 0.3
	}

	temporal := 0.5
	switch {
	case durationMs > 3000:
		temporal = 0.75
	case durationMs > 2000:
		temporal = 0.65
	case durationMs > 1000:
		temporal = 0.55
	case durationMs < 500:
		temporal = 0.3
	}

	return (semantic * 0.6) + (temporal * 0.4)
}
