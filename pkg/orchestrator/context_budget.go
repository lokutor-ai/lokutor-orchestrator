package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A call never forgets. The conversation the model sees is bounded, but nothing leaves it unless it
// has first been folded into the call's running summary, which is pinned and always sent.
//
// History has to be bounded. Prompt size is ~84ms of time to first token per 1,000 tokens
// (measured on production: 1,351 tokens -> 334ms, 3,637 -> 526ms), and input tokens are about
// three quarters of variable cost per call-minute (finance/unit-economics.html 3.3). The budget
// that bounded it until 2026-09-24 did so by deleting, and it counted the system prompt against a
// 2,500-token total. The system prompt had grown to ~1,450 tokens before the agent wrote a word,
// and to ~2,150 for a hotel agent with one knowledge document. Its history was cut to the last
// two messages on every turn from the second on, and the cuts were permanent. The agent was told
// "soy Dani" and asked for the caller's name three more times in a 65-second call. The logs said
// so plainly: context_trimmed_msgs 2, 5, 7, 7, 7, 7, 16.
//
// So, now:
//   - MaxContextTokens budgets the CONVERSATION alone. Every system message (the instructions, the
//     call summary, the knowledge-base passage) is pinned: never folded, not counted. How large an
//     agent's instructions are no longer decides how much of its call it remembers.
//   - Past the budget (or past MaxMessages), the oldest turns are handed to the summariser, and
//     only once it has written them into the summary are they removed, down to half the budget,
//     so a fold happens about once per half-budget of conversation rather than every turn.
//   - If the summariser fails or there is none, the turns stay. Over budget is a cost; forgetting
//     what the caller said mid-call is the product not working.
//
// Sizing. 800 tokens of verbatim conversation is about ten exchanges, a couple of minutes of
// talk; a call shorter than that is never summarised at all. It is sized to the worst-case
// pricing check rather than to taste (finance/pricing_model.py LLM_CAP): with a ~1,800-token
// system prompt, 300 of summary and the folds' own cost, the dearest plan minute still does not
// lose money on Business. Raising it is a price decision, not a tuning one.
const DefaultMaxContextTokens = 800

// hardHistoryCeilingTokens is the only point at which conversation is ever dropped unsummarised:
// a call that has outgrown what the model can read at all. It is reachable only if every fold of
// a call over an hour and a half has failed, and it is logged as an error when it happens, because
// by then the alternative is a model request that fails outright.
const hardHistoryCeilingTokens = 48000

// foldTimeout bounds one summarisation. A fold runs beside the call, never in front of a reply.
const foldTimeout = 20 * time.Second

// foldRetryAfter spaces out attempts after a failed fold, so a provider outage is not answered by
// a summarisation request on every message.
const foldRetryAfter = 15 * time.Second

// callSummaryPrefix opens the pinned message that holds the folded part of the call.
const callSummaryPrefix = "[Earlier in this call"

const callSummaryHeader = callSummaryPrefix + ": what was said and agreed before the messages below, condensed. " +
	"Treat it as already known and never ask the caller again for anything recorded here.]\n"

// HistorySummarizer writes the call's running record: prior is the record so far (empty on the
// first fold), turns are the oldest conversation messages to add to it. It returns the whole
// updated record.
type HistorySummarizer func(ctx context.Context, prior string, turns []Message) (string, error)

// SetHistorySummarizer lets the session fold history instead of letting it grow. Without one,
// nothing is ever folded and nothing is ever dropped below the hard ceiling.
func (s *ConversationSession) SetHistorySummarizer(f HistorySummarizer, logger Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summarizer = f
	s.foldLogger = logger
}

// isPinnedMessage reports whether a message sits outside the conversation budget and is never
// folded: every system message — the agent's instructions, the call summary, the knowledge passage.
// Instructions are not conversation, wherever they sit. On 2026-09-24 only index 0 was pinned, and a
// web/SDK client's prompt, which SetSystemPrompt then appended as a SECOND system message, was
// counted as conversation and folded into a few-token summary about a second into every session.
func isPinnedMessage(i int, m Message) bool {
	return m.Role == "system"
}

// setSystemMessage makes content the session's system prompt: it replaces the leading system
// message, or puts one first. It used to append, so a prompt set on a session that already had
// one (the web path sets the agent's, then a client's "prompt" message sets another) left two
// system prompts, the later one outside the pinned first position; SetLanguage then rebuilt the
// first from the later one's basePrompt, so the two disagreed as well.
func (s *ConversationSession) setSystemMessage(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Context) > 0 && s.Context[0].Role == "system" &&
		!strings.HasPrefix(s.Context[0].Content, callSummaryPrefix) &&
		!strings.HasPrefix(s.Context[0].Content, knowledgeContextPrefix) {
		s.Context[0].Content = content
		return
	}
	s.Context = append([]Message{{Role: "system", Content: content}}, s.Context...)
}

func messageTokens(m Message) int {
	n := estimateTokens(m.Content)
	if m.ToolCalls != nil {
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			n += len(b) / 4
		}
	}
	return n
}

// conversationLocked is the budgeted part of the context, oldest first.
func (s *ConversationSession) conversationLocked() []Message {
	conv := make([]Message, 0, len(s.Context))
	for i, m := range s.Context {
		if !isPinnedMessage(i, m) {
			conv = append(conv, m)
		}
	}
	return conv
}

func (s *ConversationSession) overBudgetLocked(conv []Message) bool {
	if s.MaxMessages > 0 && len(conv) > s.MaxMessages {
		return true
	}
	if s.MaxContextTokens <= 0 {
		return false
	}
	total := 0
	for _, m := range conv {
		total += messageTokens(m)
	}
	return total > s.MaxContextTokens
}

// foldPoint is how many of the oldest conversation messages a fold takes: enough that what remains
// is within half of each limit, never the most recent exchange, and never a boundary between a
// tool call and its results.
func foldPoint(conv []Message, maxTokens, maxMessages int) int {
	const keepRecent = 2
	if len(conv) <= keepRecent {
		return 0
	}
	targetTokens, targetCount := maxTokens/2, maxMessages/2
	remaining := 0
	for _, m := range conv {
		remaining += messageTokens(m)
	}
	k := 0
	for k < len(conv)-keepRecent {
		overTokens := maxTokens > 0 && remaining > targetTokens
		overCount := maxMessages > 0 && len(conv)-k > targetCount
		if !overTokens && !overCount {
			break
		}
		remaining -= messageTokens(conv[k])
		k++
	}
	// A tool result must stay with the call that asked for it, or the model is shown an answer to
	// nothing (and some providers reject the request outright).
	for k > 0 && k < len(conv) && (conv[k].Role == "tool" || conv[k-1].ToolCalls != nil) {
		k--
	}
	return k
}

// maybeStartFoldLocked claims a fold if one is due, returning the work to run outside the lock.
func (s *ConversationSession) maybeStartFoldLocked() func() {
	if s.summarizer == nil || s.folding || time.Now().Before(s.nextFoldAttempt) {
		return nil
	}
	conv := s.conversationLocked()
	if !s.overBudgetLocked(conv) {
		return nil
	}
	k := foldPoint(conv, s.MaxContextTokens, s.MaxMessages)
	if k == 0 {
		return nil
	}
	turns := append([]Message(nil), conv[:k]...)
	prior := s.summaryLocked()
	summarize, logger := s.summarizer, s.foldLogger
	s.folding = true
	return func() { s.runFold(summarize, logger, prior, turns) }
}

func (s *ConversationSession) runFold(summarize HistorySummarizer, logger Logger, prior string, turns []Message) {
	ctx, cancel := context.WithTimeout(context.Background(), foldTimeout)
	started := time.Now()
	summary, err := summarize(ctx, prior, turns)
	cancel()
	summary = strings.TrimSpace(summary)

	s.mu.Lock()
	s.folding = false
	if err != nil || summary == "" {
		s.nextFoldAttempt = time.Now().Add(foldRetryAfter)
		s.mu.Unlock()
		if logger != nil {
			logger.Warn("History fold failed — keeping the whole conversation; the model's prompt is over its history budget until a fold succeeds",
				"error", err, "turns", len(turns), "elapsed_ms", time.Since(started).Milliseconds())
		}
		return
	}
	applied := s.applyFoldLocked(summary, turns)
	var convTokens int
	for _, m := range s.conversationLocked() {
		convTokens += messageTokens(m)
	}
	s.mu.Unlock()
	if logger == nil {
		return
	}
	if !applied {
		logger.Info("History fold discarded: the turns it summarised changed while it ran; the next message retries",
			"turns", len(turns))
		return
	}
	logger.Info("History folded into the call summary",
		"turns_folded", len(turns), "summary_tokens", estimateTokens(summary),
		"conversation_tokens_after", convTokens, "elapsed_ms", time.Since(started).Milliseconds())
}

// applyFoldLocked replaces the call summary and removes exactly the turns it now covers. It
// refuses if those turns are no longer the oldest part of the conversation, unchanged.
func (s *ConversationSession) applyFoldLocked(summary string, turns []Message) bool {
	conv := s.conversationLocked()
	if len(conv) < len(turns) {
		return false
	}
	for i, t := range turns {
		c := conv[i]
		if c.Role != t.Role || c.Content != t.Content || c.ToolCallID != t.ToolCallID {
			return false
		}
	}
	summaryMsg := Message{Role: "system", Content: callSummaryHeader + summary}
	out := make([]Message, 0, len(s.Context)-len(turns)+1)
	dropped, placed := 0, false
	for i, m := range s.Context {
		switch {
		case i == 0 && m.Role == "system":
			out = append(out, m, summaryMsg)
			placed = true
		case m.Role == "system" && strings.HasPrefix(m.Content, callSummaryPrefix):
			// the previous summary, superseded
		case !isPinnedMessage(i, m) && dropped < len(turns):
			dropped++
		default:
			out = append(out, m)
		}
	}
	if !placed {
		out = append([]Message{summaryMsg}, out...)
	}
	s.Context = out
	s.foldedMessages += len(turns)
	return true
}

func (s *ConversationSession) summaryLocked() string {
	for _, m := range s.Context {
		if m.Role == "system" && strings.HasPrefix(m.Content, callSummaryPrefix) {
			return strings.TrimPrefix(m.Content, callSummaryHeader)
		}
	}
	return ""
}

// enforceHardCeilingLocked drops the oldest conversation, unsummarised, only past
// hardHistoryCeilingTokens. See its comment: this is the one path that loses anything.
func (s *ConversationSession) enforceHardCeilingLocked() int {
	conv := s.conversationLocked()
	total := 0
	for _, m := range conv {
		total += messageTokens(m)
	}
	if total <= hardHistoryCeilingTokens {
		return 0
	}
	k := foldPoint(conv, hardHistoryCeilingTokens*3/2, 0) // down to three quarters of the ceiling
	if k == 0 {
		return 0
	}
	out := make([]Message, 0, len(s.Context)-k)
	dropped := 0
	for i, m := range s.Context {
		if !isPinnedMessage(i, m) && dropped < k {
			dropped++
			continue
		}
		out = append(out, m)
	}
	s.Context = out
	s.trimmedMessages += k
	return k
}

// FoldedMessages is how many conversation messages this call has folded into its summary.
func (s *ConversationSession) FoldedMessages() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.foldedMessages
}

// ConversationTokens is the estimated size of the budgeted conversation, excluding the pinned
// system prompt, summary and knowledge passage.
func (s *ConversationSession) ConversationTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.conversationLocked() {
		n += messageTokens(m)
	}
	return n
}

// ContextTokens reports the estimated token cost of everything sent to the model.
func (s *ConversationSession) ContextTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.Context {
		n += messageTokens(m)
	}
	return n
}

// historySummaryInstructions is the summariser's system prompt. The record replaces the turns it
// covers, so anything it leaves out is gone for the rest of the call — hence "every concrete
// detail", values as said, and no word limit that would force one out.
const historySummaryInstructions = `You keep the running record of a live phone conversation between a caller and a voice agent. The agent reads your record instead of the turns it replaces, so anything you leave out is forgotten for the rest of the call.

Write the updated record as short plain lines, no headings or markdown. Keep every concrete detail either side gave, asked for or agreed: names and how they are spelled, phone numbers, emails, addresses, dates, times, numbers of people, prices, booking or order details, preferences, problems, what the caller still wants, and anything the agent promised, asked, or could not answer. Write each value exactly as it was said. Leave out greetings, small talk and repetition. Aim for under 150 words, but never drop a detail to get there. Write in English and keep names and values in the words used on the call.`

// summarizeHistory is the Orchestrator's HistorySummarizer: one completion on the call's own model.
func (o *Orchestrator) summarizeHistory(ctx context.Context, prior string, turns []Message) (string, error) {
	if o.llm == nil {
		return "", fmt.Errorf("no LLM provider")
	}
	var b strings.Builder
	b.WriteString("Record so far:\n")
	if strings.TrimSpace(prior) == "" {
		b.WriteString("(nothing yet)\n")
	} else {
		b.WriteString(prior)
		b.WriteString("\n")
	}
	b.WriteString("\nTurns to add, oldest first:\n")
	for _, m := range turns {
		switch m.Role {
		case "user":
			b.WriteString("Caller: " + m.Content + "\n")
		case "assistant":
			if m.Content != "" {
				b.WriteString("Agent: " + m.Content + "\n")
			}
			if m.ToolCalls != nil {
				if raw, err := json.Marshal(m.ToolCalls); err == nil {
					calls := string(raw)
					if len(calls) > 600 {
						calls = calls[:600] + "…"
					}
					b.WriteString("Agent used a tool: " + calls + "\n")
				}
			}
		case "tool":
			b.WriteString("Tool result: " + m.Content + "\n")
		default:
			b.WriteString(m.Content + "\n")
		}
	}
	b.WriteString("\nWrite the updated record.")
	return o.llm.Complete(ctx, []Message{
		{Role: "system", Content: historySummaryInstructions},
		{Role: "user", Content: b.String()},
	}, nil)
}
