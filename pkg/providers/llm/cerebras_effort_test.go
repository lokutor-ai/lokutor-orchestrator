package llm

import (
	"context"
	"testing"
)

// A model the name-based default does not know gets the effort it is given, and "" sends none.
func TestCerebrasSetReasoningEffort(t *testing.T) {
	l := NewCerebrasLLM("k", "qwen-3.8-27b")
	p := map[string]interface{}{}
	l.applyReasoningEffort(context.Background(), p)
	if _, set := p["reasoning_effort"]; set {
		t.Fatalf("qwen got a reasoning_effort by default: %v", p)
	}
	l.SetReasoningEffort(" Low ")
	p = map[string]interface{}{}
	l.applyReasoningEffort(context.Background(), p)
	if p["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", p["reasoning_effort"])
	}
	l.SetReasoningEffort("")
	p = map[string]interface{}{}
	l.applyReasoningEffort(context.Background(), p)
	if _, set := p["reasoning_effort"]; set {
		t.Fatalf("an empty effort still sent one: %v", p)
	}
}
