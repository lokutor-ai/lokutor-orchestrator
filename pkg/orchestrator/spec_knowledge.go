package orchestrator

import (
	"context"
	"strings"
	"time"
)

// knowledgeMessage is the system message a knowledge-base retrieval becomes (SetKnowledgeContext).
func knowledgeMessage(contextText string) string {
	return knowledgeContextPrefix + " Use it if relevant; do not mention that you looked it up.]\n" + contextText
}

// seedKnowledge is what the speculative seed retrieved for its words: content is "" when nothing matched.
type seedKnowledge struct {
	seed    string
	content string
	at      time.Time
}

// seedKnowledgeRequest gives a speculative seed the knowledge-base passage its turn will have, so the guess is asked
// what the turn will be asked. The seed used to be asked with the conversation as it stood, carrying the previous
// turn's passage, while the turn itself retrieved for its own words after the caller stopped and then used the guess
// anyway: a knowledge-base agent's speculative reply answered from the last question's passage (2026-10-16 review).
// It returns the history and the messages that follow the caller's words, as injectRagContext arranges them: a
// passage replaces the earlier one and comes after the user message; nothing matched leaves the conversation as it is.
// The retrieval is kept for injectRagContext, which then has nothing to fetch on the critical path. A failed retrieval
// keeps nothing: the turn retrieves again, and its guess is not used (knowledgeFromSeed).
func (ms *ManagedStream) seedKnowledgeRequest(ctx context.Context, seed string, history []Message) ([]Message, []Message) {
	if ms.orch == nil || ms.orch.rag == nil {
		return history, nil
	}
	content, err := ms.orch.rag.Retrieve(ctx, seed)
	if err != nil {
		return history, nil
	}
	ms.mu.Lock()
	ms.specKnowledge = &seedKnowledge{seed: seed, content: content, at: time.Now()}
	ms.mu.Unlock()
	if content == "" {
		return history, nil
	}
	kept := make([]Message, 0, len(history))
	for i, m := range history {
		if i > 0 && m.Role == "system" && strings.HasPrefix(m.Content, knowledgeContextPrefix) {
			continue
		}
		kept = append(kept, m)
	}
	return kept, []Message{{Role: "system", Content: knowledgeMessage(content)}}
}
