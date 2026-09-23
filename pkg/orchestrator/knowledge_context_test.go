package orchestrator

import (
	"strings"
	"testing"
)

// 2026-09-23, hotel agent on the phone: the caller gave their name on turn two and was asked for it
// again on turn seven. Every turn's knowledge-base retrieval (650–1,300 characters) was appended and
// kept, and the 2,500-token budget trims oldest-first, so the stale passages stayed and the
// conversation went. One knowledge slot, replaced each turn, keeps the conversation.
func TestKnowledgeContextDoesNotCrowdOutTheConversation(t *testing.T) {
	s := NewConversationSession("kb")
	s.MaxContextTokens = DefaultMaxContextTokens
	s.AddMessageRaw(Message{Role: "system", Content: strings.Repeat("s", 1421*4)}) // this agent's system prompt
	s.AddMessageRaw(Message{Role: "assistant", Content: "¡Hola! Bienvenido al Hotel Estrella, ¿con quién tengo el placer de hablar?"})
	s.AddMessageRaw(Message{Role: "user", Content: "Me llamo Manolo."})
	s.AddMessageRaw(Message{Role: "assistant", Content: "Vale, Manolo. ¿Qué fechas tenías en mente para tu estancia?"})
	for turn := 0; turn < 6; turn++ {
		s.SetKnowledgeContext(knowledgeContextPrefix + " Use it if relevant.]\n" + strings.Repeat("k", 1300))
		s.AddMessageRaw(Message{Role: "user", Content: "Eh, pues no sé. O sea, ¿qué me recomiendas?"})
		s.AddMessageRaw(Message{Role: "assistant", Content: "Te sugiero llegar el viernes y quedarte dos noches. ¿Te va bien?"})
	}

	ctx := s.GetContextCopy()
	knowledge, named := 0, false
	for _, m := range ctx {
		if strings.HasPrefix(m.Content, knowledgeContextPrefix) {
			knowledge++
		}
		if strings.Contains(m.Content, "Manolo") {
			named = true
		}
	}
	if knowledge != 1 {
		t.Fatalf("%d knowledge-base messages in context, want exactly the latest one", knowledge)
	}
	if !named {
		t.Fatalf("the caller's name was trimmed out after six turns (trimmed %d messages)", s.TrimmedMessages())
	}
}
