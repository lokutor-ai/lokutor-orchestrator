package orchestrator

import (
	"strings"
)

type TurnCompletionAnalyzer struct{}

func NewTurnCompletionAnalyzer() *TurnCompletionAnalyzer {
	return &TurnCompletionAnalyzer{}
}

func (tca *TurnCompletionAnalyzer) IsLikelyComplete(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	lastRune := lastVisibleRune(text)

	if lastRune == '?' || lastRune == '!' || lastRune == '¿' || lastRune == '¡' {
		return true
	}

	if lastRune == '.' {
		words := strings.Fields(text)
		if len(words) >= 2 {
			return true
		}
		return false
	}

	return false
}

func (tca *TurnCompletionAnalyzer) ProsodyIndicatesCompletion(vad VADProvider) float64 {
	if improvedVAD, ok := vad.(*ImprovedRMSVAD); ok {
		trend := improvedVAD.GetEnergyTrend()
		if trend < -0.05 {
			return 0.8
		} else if trend > 0.05 {
			return 0.2
		}
		return 0.5
	}
	return 0.5
}

func (tca *TurnCompletionAnalyzer) CombinedCompletionScore(
	text string,
	durationMs int,
	vad VADProvider,
) float64 {
	semanticScore := 0.5
	if tca.IsLikelyComplete(text) {
		semanticScore = 0.7
	} else {
		semanticScore = 0.3
	}

	prosodyScore := tca.ProsodyIndicatesCompletion(vad)

	temporalScore := 0.5
	if durationMs > 3000 {
		temporalScore = 0.75
	} else if durationMs > 2000 {
		temporalScore = 0.65
	} else if durationMs > 1000 {
		temporalScore = 0.55
	} else if durationMs < 500 {
		temporalScore = 0.3
	}

	return (semanticScore * 0.5) + (temporalScore * 0.3) + (prosodyScore * 0.2)
}

func lastVisibleRune(s string) rune {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) == 0 {
		return 0
	}
	return runes[len(runes)-1]
}
