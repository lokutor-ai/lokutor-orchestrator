package orchestrator

import (
	"testing"
)

func TestTurnCompletionAnalyzerEmpty(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	if tca.IsLikelyComplete("") {
		t.Error("Expected false for empty string")
	}
}

func TestTurnCompletionAnalyzerQuestionMark(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	if !tca.IsLikelyComplete("What time is it?") {
		t.Error("Expected true for question ending with ?")
	}
	if !tca.IsLikelyComplete("¿Qué hora es?") {
		t.Error("Expected true for Spanish question")
	}
}

func TestTurnCompletionAnalyzerExclamation(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	if !tca.IsLikelyComplete("Wow!") {
		t.Error("Expected true for exclamation")
	}
	if !tca.IsLikelyComplete("¡Increíble!") {
		t.Error("Expected true for Spanish exclamation")
	}
}

func TestTurnCompletionAnalyzerPeriod(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	if !tca.IsLikelyComplete("I'm fine.") {
		t.Error("Expected true for sentence ending with period")
	}
	if tca.IsLikelyComplete(".") {
		t.Error("Expected false for just a period")
	}
	if tca.IsLikelyComplete("Dr.") {
		t.Error("Expected false for abbreviation with single word")
	}
}

func TestTurnCompletionAnalyzerNoPunct(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	// Without terminal punctuation, it's incomplete
	if tca.IsLikelyComplete("I'm fine") {
		t.Error("Expected false for text without terminal punctuation")
	}
}

func TestCombinedCompletionScore(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	score := tca.CombinedCompletionScore("Hello?", 1500, nil)
	if score <= 0 {
		t.Error("Expected positive score")
	}
	if score > 1.0 {
		t.Error("Expected score <= 1.0")
	}
}

func TestCombinedCompletionScoreLongDuration(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	// Longer utterances get higher temporal scores
	shortScore := tca.CombinedCompletionScore("Hi.", 400, nil)
	longScore := tca.CombinedCompletionScore("Hi.", 4000, nil)
	if longScore <= shortScore {
		t.Error("Expected longer duration to score higher")
	}
}

func TestLastVisibleRune(t *testing.T) {
	tests := []struct {
		input string
		want  rune
	}{
		{"Hello.", '.'},
		{"Hello world", 'd'},
		{"  ", 0},
		{"", 0},
		{"¿Qué?", '?'},
	}

	for _, tt := range tests {
		got := lastVisibleRune(tt.input)
		if got != tt.want {
			t.Errorf("lastVisibleRune(%q) = %c, want %c", tt.input, got, tt.want)
		}
	}
}
