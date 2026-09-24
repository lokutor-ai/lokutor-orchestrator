package orchestrator

import (
	"fmt"
	"strings"
	"testing"
)

// The count cap used to be a plain tail slice, which dropped index 0 — the system prompt, carrying
// the whole language section, the guardrails and the agent's own instructions — and later a slice
// that kept the system prompt but still deleted the conversation. Neither may happen: past the cap
// the oldest turns fold into the summary, and the system prompt stays first.
func TestSystemPromptSurvivesFolding(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("s")
	s.MaxMessages = 8
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM: Always respond in Catalan.")
	for i := 0; i < 40; i++ {
		s.AddMessage("user", fmt.Sprintf("user turn %d", i))
		s.AddMessage("assistant", fmt.Sprintf("assistant turn %d", i))
		waitIdle(s)
	}
	waitIdle(s)

	ctx := s.GetContextCopy()
	if ctx[0].Role != "system" || !strings.Contains(ctx[0].Content, "Always respond in Catalan") {
		t.Fatalf("first message is %q — the language rules are no longer first", ctx[0].Content)
	}
	if got := len(s.conversationLocked()); got > s.MaxMessages+2 {
		t.Errorf("conversation is %d messages, past the cap of %d by more than one exchange", got, s.MaxMessages)
	}
	if !strings.Contains(ctx[len(ctx)-1].Content, "39") {
		t.Errorf("most recent message is %q — the newest turns went instead of the oldest", ctx[len(ctx)-1].Content)
	}
	if !strings.Contains(contextText(s), "user turn 0") {
		t.Error("the first turn is gone from both the conversation and the summary")
	}
}

// A session with no system message folds too, and the summary leads.
func TestFoldWithoutASystemPrompt(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("s")
	s.MaxMessages = 5
	s.SetHistorySummarizer(sum.summarize, nil)
	for i := 0; i < 20; i++ {
		s.AddMessage("user", fmt.Sprintf("m%d", i))
		waitIdle(s)
	}
	ctx := s.GetContextCopy()
	if !strings.HasPrefix(ctx[0].Content, callSummaryPrefix) {
		t.Fatalf("first message is %q, want the call summary", ctx[0].Content)
	}
	if !strings.Contains(ctx[0].Content, "m0") {
		t.Error("the first message is not in the summary")
	}
}

// MaxMessages of 1 with a system prompt must not produce an empty or malformed context.
func TestDegenerateMaxMessages(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("s")
	s.MaxMessages = 1
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM")
	s.AddMessage("user", "hello")
	waitIdle(s)
	ctx := s.GetContextCopy()
	if len(ctx) == 0 || ctx[0].Role != "system" || ctx[0].Content != "SYSTEM" {
		t.Fatalf("context malformed at MaxMessages=1: %+v", ctx)
	}
	if !strings.Contains(contextText(s), "hello") {
		t.Error("the only turn was lost")
	}
}
