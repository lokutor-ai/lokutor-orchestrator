package orchestrator

// Conversation context is capped by TOKENS, not only by message count.
//
// Message count is a poor proxy for what context actually costs. One long turn can carry more than
// twenty short ones, and the price is paid twice over:
//
//   - Latency. Measured on production across 18 turns, time to FIRST TOKEN scales with prompt size
//     at roughly 84ms per 1,000 prompt tokens (1,351 tokens -> 334ms; 3,637 -> 526ms). A call
//     observed at 7,146 prompt tokens was therefore paying about 600ms of time-to-first-audio for
//     prompt size alone, on a turn budget where the whole response target is under a second.
//   - Money. Input tokens are roughly 76% of variable cost per call-minute (see
//     finance/unit-economics.html 3.3), so context growth is the single largest controllable line.
//
// Nothing capped it. summarizeContextIfNeeded, the function written to do exactly this, is called
// from nowhere — dead code — so the only ceiling was MaxMessages at 100, and reaching that ceiling
// used to destroy the system prompt (see AddMessageRaw).
//
// The budget is deliberately not enormous. DefaultMaxContextTokens leaves room for the system
// prompt (around a thousand tokens on its own) plus roughly twenty short turns of history, which
// is far more recent context than a voice conversation refers back to, while holding the prompt's
// latency contribution near 200ms rather than 600ms.
//
// Trimming is oldest-first and never touches the system message: the agent's instructions and the
// language rules are the part that must not age out. A caller who needs more history than this
// should raise the budget explicitly and accept the latency, which is now a visible trade rather
// than an accident.
const DefaultMaxContextTokens = 2500

// trimToTokenBudgetLocked drops the oldest non-system messages until the conversation fits
// MaxContextTokens. The caller must hold s.mu.
func (s *ConversationSession) trimToTokenBudgetLocked() {
	if s.MaxContextTokens <= 0 || len(s.Context) == 0 {
		return
	}

	// The system message is pinned and its cost is unavoidable, so it is charged against the
	// budget but never trimmed. If it alone exceeds the budget the history goes to nothing rather
	// than the rules going away — a prompt with no history still behaves correctly, a conversation
	// with no rules does not.
	start := 0
	total := 0
	if s.Context[0].Role == "system" {
		start = 1
		total += estimateTokens(s.Context[0].Content)
	}

	// Walk backwards from the newest message, keeping what fits.
	keepFrom := len(s.Context)
	for i := len(s.Context) - 1; i >= start; i-- {
		cost := estimateTokens(s.Context[i].Content)
		if total+cost > s.MaxContextTokens {
			break
		}
		total += cost
		keepFrom = i
	}

	if keepFrom <= start {
		return // everything already fits
	}

	// Always keep at least the most recent exchange. A turn answered with no conversational
	// context at all is worse than one slightly over budget, and this is the case a very large
	// system prompt would otherwise produce.
	minKeep := 2
	if avail := len(s.Context) - start; avail < minKeep {
		minKeep = avail
	}
	if len(s.Context)-keepFrom < minKeep {
		keepFrom = len(s.Context) - minKeep
	}

	s.trimmedMessages += keepFrom - start
	trimmed := make([]Message, 0, 1+len(s.Context)-keepFrom)
	if start == 1 {
		trimmed = append(trimmed, s.Context[0])
	}
	trimmed = append(trimmed, s.Context[keepFrom:]...)
	s.Context = trimmed
}

// ContextTokens reports the estimated token cost of the conversation as it stands. Exported so the
// turn logger and tests can see the number the budget is acting on.
func (s *ConversationSession) ContextTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.Context {
		n += estimateTokens(m.Content)
	}
	return n
}
