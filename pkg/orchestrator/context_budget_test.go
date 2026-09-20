package orchestrator

import (
	"fmt"
	"strings"
	"testing"
)

// Context was uncapped in practice: summarizeContextIfNeeded is dead code, so the only ceiling was
// MaxMessages, and a real call was measured at 7,146 prompt tokens and still climbing. At the
// measured ~84ms of first-token latency per 1,000 prompt tokens that is ~600ms of
// time-to-first-audio spent on prompt size alone.
func TestContextStaysWithinTheTokenBudget(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxMessages = 1000 // take the count cap out of the picture
	s.MaxContextTokens = 800
	s.AddMessage("system", strings.Repeat("SYSTEM RULES. ", 20)) // ~70 tokens

	for i := 0; i < 200; i++ {
		s.AddMessage("user", fmt.Sprintf("a fairly ordinary spoken sentence number %d, long enough to cost tokens", i))
		s.AddMessage("assistant", fmt.Sprintf("and a reply of similar length to sentence %d, as a real turn would be", i))
	}

	if got := s.ContextTokens(); got > s.MaxContextTokens {
		t.Errorf("context is %d tokens, above the %d budget", got, s.MaxContextTokens)
	}
	ctx := s.GetContextCopy()
	if ctx[0].Role != "system" {
		t.Fatal("system prompt was trimmed — the language rules and agent instructions aged out")
	}
	if !strings.Contains(ctx[len(ctx)-1].Content, "199") {
		t.Error("newest turn was trimmed instead of the oldest")
	}
}

// A system prompt larger than the whole budget must not leave the turn with no conversation. The
// rules survive and history shrinks, never the reverse.
func TestOversizedSystemPromptKeepsRecentTurns(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxContextTokens = 50
	s.AddMessage("system", strings.Repeat("VERY LONG SYSTEM PROMPT. ", 200))
	s.AddMessage("user", "hola")
	s.AddMessage("assistant", "hola, digam")

	ctx := s.GetContextCopy()
	if ctx[0].Role != "system" {
		t.Fatal("system prompt dropped in favour of history — exactly backwards")
	}
	if len(ctx) < 2 {
		t.Error("no conversational context survived; a turn answered with none is worse than one over budget")
	}
}

// Zero disables the token cap, so an operator can opt out without editing code.
func TestZeroBudgetDisablesTokenTrim(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxMessages = 1000
	s.MaxContextTokens = 0
	for i := 0; i < 100; i++ {
		s.AddMessage("user", strings.Repeat("x", 400))
	}
	if got := len(s.GetContextCopy()); got != 100 {
		t.Errorf("got %d messages, want all 100 — a zero budget must not trim", got)
	}
}

// The default has to be small enough to matter. A budget above what was already observed in
// production would cap nothing.
func TestDefaultBudgetIsBelowWhatWasObserved(t *testing.T) {
	const observedOnARealCall = 7146
	if DefaultMaxContextTokens >= observedOnARealCall {
		t.Errorf("DefaultMaxContextTokens=%d does not cap the %d tokens already measured in production",
			DefaultMaxContextTokens, observedOnARealCall)
	}
}
