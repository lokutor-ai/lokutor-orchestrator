package llm

import "testing"

// reasoning_effort is not a universally accepted parameter: Groq returns 400
// for models outside the gpt-oss family. Getting the gating wrong therefore
// breaks every LLM turn rather than merely running slower, which is why it is
// worth a test of its own.
func TestDefaultReasoningEffort(t *testing.T) {
	t.Setenv("GROQ_REASONING_EFFORT", "")

	cases := []struct {
		model string
		want  string
	}{
		{"openai/gpt-oss-120b", "low"},
		{"openai/gpt-oss-20b", "low"},
		{"meta-llama/llama-4-scout-17b-16e-instruct", ""},
		{"qwen/qwen3.8-27b", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := defaultReasoningEffort(c.model); got != c.want {
			t.Errorf("defaultReasoningEffort(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

func TestReasoningEffortEnvOverride(t *testing.T) {
	t.Setenv("GROQ_REASONING_EFFORT", "high")
	if got := defaultReasoningEffort("openai/gpt-oss-120b"); got != "high" {
		t.Errorf("override = %q, want high", got)
	}
	// The override has to reach models that would otherwise be left alone, so
	// a deliberate opt-in is not silently dropped.
	if got := defaultReasoningEffort("qwen/qwen3.8-27b"); got != "high" {
		t.Errorf("override on non-gpt-oss = %q, want high", got)
	}

	// "default"/"off" mean "send nothing" — the escape hatch back to API
	// behaviour without a rebuild.
	for _, v := range []string{"default", "off", "DEFAULT"} {
		t.Setenv("GROQ_REASONING_EFFORT", v)
		if got := defaultReasoningEffort("openai/gpt-oss-120b"); got != "" {
			t.Errorf("GROQ_REASONING_EFFORT=%q gave %q, want empty", v, got)
		}
	}
}

// The payload must simply not carry the key when the effort is empty; an
// explicit null or "" would be a 400 just the same as the wrong model.
func TestApplyReasoningEffortOmitsWhenUnset(t *testing.T) {
	l := &GroqLLM{reasoningEffort: ""}
	p := map[string]interface{}{"model": "x"}
	l.applyReasoningEffort(p)
	if _, ok := p["reasoning_effort"]; ok {
		t.Error("reasoning_effort present in payload when unset")
	}

	l = &GroqLLM{reasoningEffort: "low"}
	p = map[string]interface{}{"model": "x"}
	l.applyReasoningEffort(p)
	if p["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", p["reasoning_effort"])
	}
}
