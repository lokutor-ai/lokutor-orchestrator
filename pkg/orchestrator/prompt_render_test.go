package orchestrator

import (
	"strings"
	"testing"
)

// The system prompt is built with fmt.Sprintf and a growing number of %s verbs. A miscount does not
// fail the build — it renders "%!s(MISSING)" into the live instructions, or silently drops the
// caller's own prompt. Both ship a broken agent quietly.
func TestSystemPromptRendersCleanly(t *testing.T) {
	out := buildSystemPrompt("BE A HELPFUL TEST AGENT", "Spanish")

	for _, bad := range []string{"%!s(MISSING)", "%!(EXTRA", "%s"} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered prompt contains %q — the format verbs and arguments disagree:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "BE A HELPFUL TEST AGENT") {
		t.Error("the caller's own prompt is missing from the rendered output")
	}
	if n := strings.Count(out, "Spanish"); n < 5 {
		t.Errorf("language named only %d times; the language rule depends on it being stated throughout", n)
	}
	// The two rules that a live call proved are load-bearing.
	for _, want := range []string{"never mix languages inside one reply", "Never end a call on a short, ambiguous"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing rule: %q", want)
		}
	}
}
