package orchestrator

import (
	"strings"
	"testing"
)

func roles(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteByte(m.Role[0])
	}
	return b.String()
}

// The step message sits right after the system prompt; changing it leaves the system prompt and the conversation as they
// were, and nothing that rebuilds the system prompt takes it for the system prompt.
func TestStepPrompt(t *testing.T) {
	o := New(nil, nil, nil, DefaultConfig())
	s := o.NewSessionWithDefaults("s")
	s.SetStepPrompt("# Step: one") // before there is a system prompt
	o.SetSystemPrompt(s, "You are Lucía.")
	if c := s.GetContextCopy(); roles(c) != "ss" || !strings.Contains(c[0].Content, "You are Lucía.") || c[1].Content != "# Step: one" {
		t.Fatalf("context %s: %q", roles(c), c)
	}
	s.AddMessage("user", "Hola")
	s.AddMessage("assistant", "Buenas")
	system := s.GetContextCopy()[0].Content
	s.SetStepPrompt("# Step: two")
	c := s.GetContextCopy()
	if roles(c) != "ssua" || c[0].Content != system || c[1].Content != "# Step: two" || c[2].Content != "Hola" {
		t.Fatalf("after a change %s: %q", roles(c), c)
	}
	o.SetLanguage(s, LanguageEs)
	o.SetSystemPrompt(s, "You are Lucía, again.")
	s.SetKnowledgeContext(knowledgeMessage("abrimos a las ocho"))
	c = s.GetContextCopy()
	if roles(c) != "ssuas" || !strings.Contains(c[0].Content, "You are Lucía, again.") || !strings.Contains(c[0].Content, "Spanish") || c[1].Content != "# Step: two" {
		t.Fatalf("after the system prompt was rebuilt %s: %q", roles(c), c)
	}
	if s.StepPrompt() != "# Step: two" {
		t.Fatal(s.StepPrompt())
	}
	s.SetStepPrompt("")
	if c = s.GetContextCopy(); roles(c) != "suas" || s.StepPrompt() != "" {
		t.Fatalf("not removed %s: %q", roles(c), c)
	}
}
