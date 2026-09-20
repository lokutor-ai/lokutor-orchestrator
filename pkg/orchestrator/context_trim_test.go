package orchestrator

import (
	"fmt"
	"strings"
	"testing"
)

// The trim used to be a plain tail slice, which drops index 0 — the system prompt, carrying the
// whole language section, the guardrails and the agent's own instructions. A call long enough to
// reach MaxMessages lost every rule holding it to one language, silently, mid-conversation.
//
// Not hypothetical head-room: context on a real call was measured past 7,000 tokens and still
// climbing, because summarizeContextIfNeeded — the function meant to cap it — is called from
// nowhere.
func TestSystemPromptSurvivesContextTrim(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxMessages = 8
	s.AddMessage("system", "SYSTEM: Always respond in Catalan.")
	for i := 0; i < 40; i++ {
		s.AddMessage("user", fmt.Sprintf("user turn %d", i))
		s.AddMessage("assistant", fmt.Sprintf("assistant turn %d", i))
	}

	ctx := s.GetContextCopy()
	if len(ctx) > s.MaxMessages {
		t.Fatalf("context grew to %d, above MaxMessages %d", len(ctx), s.MaxMessages)
	}
	if ctx[0].Role != "system" {
		t.Fatalf("first message is %q, not the system prompt — the language rules were trimmed away",
			ctx[0].Role)
	}
	if !strings.Contains(ctx[0].Content, "Always respond in Catalan") {
		t.Error("the system prompt survived in position but lost its content")
	}
	// The most recent exchange must still be there; keeping the system message is not a licence to
	// drop the conversation.
	last := ctx[len(ctx)-1].Content
	if !strings.Contains(last, "39") {
		t.Errorf("most recent message is %q — the newest turns were trimmed instead of the oldest", last)
	}
}

// A session with no system message must still trim to the cap rather than growing unbounded.
func TestTrimStillAppliesWithoutASystemPrompt(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxMessages = 5
	for i := 0; i < 20; i++ {
		s.AddMessage("user", fmt.Sprintf("m%d", i))
	}
	if got := len(s.GetContextCopy()); got != 5 {
		t.Errorf("context is %d messages, want the cap of 5", got)
	}
}

// MaxMessages of 1 with a system prompt must not produce an empty or malformed context.
func TestDegenerateMaxMessages(t *testing.T) {
	s := NewConversationSession("s")
	s.MaxMessages = 1
	s.AddMessage("system", "SYSTEM")
	s.AddMessage("user", "hello")
	ctx := s.GetContextCopy()
	if len(ctx) == 0 {
		t.Fatal("context emptied itself")
	}
	if ctx[0].Role != "system" {
		t.Errorf("system prompt lost at MaxMessages=1; first role is %q", ctx[0].Role)
	}
}
