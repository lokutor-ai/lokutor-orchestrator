package orchestrator

import (
	"regexp"
	"strings"
)

// TurnCompletionAnalyzer uses research-backed linguistic and prosodic features
// to determine if a speech turn is actually complete or just a pause.
// Based on:
// - Prosodic Turn-Taking Cues (Cutler & Pearson 2018)
// - End-of-Turn Detection for Speech Agents (Arsikere et al. 2015)
// - Turn-End Estimation in Conversational Turn-Taking (Bögels & Torreira 2021)
type TurnCompletionAnalyzer struct {
	// Features that indicate sentence completeness
	incompletePhrasePatterns []*regexp.Regexp
	completionMarkers        []*regexp.Regexp
}

func NewTurnCompletionAnalyzer() *TurnCompletionAnalyzer {
	tca := &TurnCompletionAnalyzer{
		// Patterns indicating incomplete/continuing speech
		incompletePhrasePatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\band\s*$`),        // "and "
			regexp.MustCompile(`(?i)\bor\s*$`),         // "or "
			regexp.MustCompile(`(?i)\bbut\s*$`),        // "but "
			regexp.MustCompile(`(?i)\bbecause\s*$`),    // "because "
			regexp.MustCompile(`(?i)\blike\s*$`),       // "like " (filler)
			regexp.MustCompile(`(?i)\byou\s+know\s*$`), // "you know "
			regexp.MustCompile(`(?i)\bI\s+mean\s*$`),   // "I mean "
			regexp.MustCompile(`(?i),\s*$`),            // ends with comma
			regexp.MustCompile(`(?i)\.\.\.\s*$`),       // ellipsis
			regexp.MustCompile(`(?i)\bwhich\s*$`),      // relative clause start
			regexp.MustCompile(`(?i)\bthat\s*$`),       // clause continuation
			regexp.MustCompile(`(?i)\bwhen\s*$`),       // temporal clause
			regexp.MustCompile(`(?i)\bif\s*$`),         // conditional
			regexp.MustCompile(`(?i)\bso\s*$`),         // continuation marker
		},
		// Patterns indicating definite sentence completion
		completionMarkers: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\?\s*$`),      // question mark
			regexp.MustCompile(`(?i)!\s*$`),       // exclamation
			regexp.MustCompile(`(?i)\.\s*$`),      // period
			regexp.MustCompile(`(?i)right\?\s*$`), // "right?" tag question
			regexp.MustCompile(`(?i)yeah\s*$`),    // yeah (affirm)
			regexp.MustCompile(`(?i)okay\s*$`),    // okay (affirmative)
			regexp.MustCompile(`(?i)sure\s*$`),    // sure (affirmative)
		},
	}
	return tca
}

// IsLikelyComplete analyzes the transcribed text to predict if the turn is complete.
// Returns false if the text shows signs of incompleteness (high probability of continuing).
func (tca *TurnCompletionAnalyzer) IsLikelyComplete(transcribedText string) bool {
	text := strings.TrimSpace(transcribedText)
	if text == "" {
		return false // Empty text is incomplete
	}

	// Check for explicit completion markers (high confidence end)
	for _, pattern := range tca.completionMarkers {
		if pattern.MatchString(text) {
			return true // Definite end marker
		}
	}

	// Check for incomplete/continuation patterns
	// If ANY incompleteness pattern matches, it's likely continuing
	for _, pattern := range tca.incompletePhrasePatterns {
		if pattern.MatchString(text) {
			return false // Strong signal of incompleteness
		}
	}

	// Heuristics for structural hints
	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}

	lastWord := strings.ToLower(words[len(words)-1])

	// Single words or very short utterances often continue
	if len(words) <= 2 {
		// Unless it's a clear affirmation
		affirms := map[string]bool{"yes": true, "yeah": true, "okay": true, "ok": true, "sure": true, "nope": true, "no": true}
		if affirms[strings.ToLower(strings.Trim(lastWord, ".,!?"))] {
			return true
		}
		return false // Likely continuing
	}

	// If the text ends with an article or pronoun, likely incomplete
	incompleteParts := map[string]bool{
		"a": true, "an": true, "the": true,
		"i": true, "you": true, "he": true, "she": true, "we": true, "they": true,
		"this": true, "that": true, "these": true, "those": true,
		"is": true, "are": true, "was": true, "were": true,
		"would": true, "could": true, "should": true, "might": true,
	}
	lastWordClean := strings.Trim(lastWord, ".,!?")
	if incompleteParts[lastWordClean] {
		return false // Incomplete
	}

	// Default: if no clear markers, assume complete (we'll rely on prosody and utterance length)
	return true
}

// CombinedCompletionScore combines semantic and temporal signals.
// Returns a score 0-1 where:
// - < 0.4: Very likely continuing
// - 0.4-0.6: Ambiguous
// - > 0.6: Very likely complete
func (tca *TurnCompletionAnalyzer) CombinedCompletionScore(
	transcribedText string,
	utteranceDuration int, // in milliseconds
) float64 {
	semanticScore := 0.5
	if tca.IsLikelyComplete(transcribedText) {
		semanticScore = 0.7
	} else {
		semanticScore = 0.3
	}

	// Temporal heuristic: longer utterances are more likely complete
	temporalScore := 0.5
	if utteranceDuration > 3000 { // > 3 seconds
		temporalScore = 0.75
	} else if utteranceDuration > 2000 { // > 2 seconds
		temporalScore = 0.65
	} else if utteranceDuration > 1000 { // > 1 second
		temporalScore = 0.55
	} else if utteranceDuration < 500 { // < 0.5 seconds
		temporalScore = 0.3
	}

	// Weight: Semantic (60%) + Temporal (40%)
	// Semantic is weighted more because NLU is strongest signal
	combined := (semanticScore * 0.6) + (temporalScore * 0.4)

	return combined
}
