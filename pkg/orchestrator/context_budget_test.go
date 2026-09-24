package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A call must never lose what was said in it. These tests hold the contract context_budget.go
// states: the conversation is bounded by folding its oldest turns into a pinned call summary, and
// nothing leaves the context unless it is in that summary.

// recordingSummarizer is a HistorySummarizer that keeps every turn's text in the record, so a test
// can check that whatever was folded is still in front of the model.
type recordingSummarizer struct {
	mu    sync.Mutex
	calls int
	err   error
	block chan struct{} // when set, a fold waits on it
}

func (r *recordingSummarizer) summarize(ctx context.Context, prior string, turns []Message) (string, error) {
	r.mu.Lock()
	r.calls++
	err, block := r.err, r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(prior)
	for _, t := range turns {
		b.WriteString("\n" + t.Role + ": " + t.Content)
	}
	return b.String(), nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitIdle(s *ConversationSession) {
	for i := 0; i < 1000; i++ {
		s.mu.RLock()
		busy := s.folding
		s.mu.RUnlock()
		if !busy {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func contextText(s *ConversationSession) string {
	var b strings.Builder
	for _, m := range s.GetContextCopy() {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

// 2026-09-24, verbatim sizes from the hotel call that asked "Dani" for their name three more times:
// an 8,621-byte system prompt (about 2,150 estimated tokens: the platform's instructions plus one
// pasted knowledge document), a 1,300-byte knowledge passage, and seven short exchanges. Against the
// old 2,500-token total the history was cut to the last two messages from the second turn on. The
// system prompt and the passage must not count against the conversation at all.
func TestLongInstructionsDoNotEatTheConversation(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("hotel")
	s.MaxMessages = 100
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", strings.Repeat("s", 8621))
	s.AddMessage("user", OpeningTrigger)
	s.AddMessage("assistant", "¡Hola! Bienvenido al Hotel Sol, ¿con quién tengo el gusto de hablar?")
	turns := [][2]string{
		{"Ah, soy Dani. Me gustaría hacer una reserva.", "¡Encantado, Dani! ¿Para qué fechas te gustaría la reserva?"},
		{"uh no sé cuéntese disponible", "Vale, ¿qué te gustaría saber sobre la disponibilidad?"},
		{"It's Japanese simulation.", "Perdona, ¿puedes repetirlo en español?"},
		{"Estas tenéis disponibles.", "No puedo confirmar la disponibilidad ahora mismo, pero te tomo nota de las fechas."},
		{"Mi nombre.", "Te llamas Dani, ¿verdad?"},
		{"Llamado Dani.", "Perfecto, Dani. ¿Qué fechas te interesan?"},
		{"eh, no lo sé qué fechas tenéis disponibles.", "Dime unas fechas aproximadas y lo dejo anotado."},
	}
	for _, turn := range turns {
		s.SetKnowledgeContext(knowledgeContextPrefix + " Use it if relevant.]\n" + strings.Repeat("k", 1300))
		s.AddMessage("user", turn[0])
		s.AddMessage("assistant", turn[1])
	}
	waitIdle(s)

	if sum.calls != 0 {
		t.Errorf("a 65-second call was summarised %d times; it is well inside its conversation budget", sum.calls)
	}
	if n := s.TrimmedMessages(); n != 0 {
		t.Errorf("%d messages dropped", n)
	}
	if !strings.Contains(contextText(s), "Ah, soy Dani") {
		t.Fatal("the turn where the caller gave their name is gone")
	}
	if got := len(s.conversationLocked()); got != 2+2*len(turns) {
		t.Errorf("conversation has %d messages, want all %d", got, 2+2*len(turns))
	}
}

// Past the budget the oldest turns are folded, and what was folded is still in front of the model.
func TestFoldKeepsEverythingAndBoundsTheConversation(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("long")
	s.MaxMessages = 1000
	s.MaxContextTokens = 400
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM RULES")
	s.AddMessage("user", "Me llamo Dani Varela, D-A-N-I.")
	s.AddMessage("assistant", "Encantado, Dani.")
	for i := 0; i < 120; i++ {
		s.AddMessage("user", fmt.Sprintf("an ordinary spoken sentence number %d, long enough to cost tokens", i))
		s.AddMessage("assistant", fmt.Sprintf("and a reply of similar length to sentence %d, as a real turn would be", i))
		waitIdle(s) // one fold at a time, as on a call where turns are seconds apart
	}
	waitIdle(s)

	ctx := s.GetContextCopy()
	if ctx[0].Content != "SYSTEM RULES" {
		t.Fatal("the system prompt is no longer first")
	}
	if !strings.HasPrefix(ctx[1].Content, callSummaryPrefix) {
		t.Fatalf("second message is %q, want the call summary", ctx[1].Content[:30])
	}
	if !strings.Contains(ctx[1].Content, "D-A-N-I") {
		t.Error("the caller's name, folded, is not in the summary the model reads")
	}
	if !strings.Contains(ctx[len(ctx)-1].Content, "119") {
		t.Error("the newest turn is not last")
	}
	if n := s.TrimmedMessages(); n != 0 {
		t.Errorf("%d messages dropped without a summary", n)
	}
	if s.FoldedMessages() == 0 {
		t.Fatal("nothing was folded")
	}
	// Bounded: at most the budget plus the exchange that tipped it over.
	if got := s.ConversationTokens(); got > s.MaxContextTokens+60 {
		t.Errorf("conversation is %d tokens against a %d budget", got, s.MaxContextTokens)
	}
	// Every sentence ever said is either verbatim or in the summary.
	text := contextText(s)
	for i := 0; i < 120; i++ {
		if !strings.Contains(text, fmt.Sprintf("sentence number %d,", i)) {
			t.Fatalf("sentence %d is nowhere in the context", i)
		}
	}
}

// A summariser that fails must cost budget, never memory.
func TestFailedFoldKeepsTheWholeConversation(t *testing.T) {
	sum := &recordingSummarizer{err: errors.New("provider down")}
	s := NewConversationSession("down")
	s.MaxMessages = 1000
	s.MaxContextTokens = 100
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM")
	for i := 0; i < 40; i++ {
		s.AddMessage("user", fmt.Sprintf("caller sentence %d with some words in it", i))
		waitIdle(s)
	}
	if sum.calls == 0 {
		t.Fatal("no fold was attempted")
	}
	if got := len(s.conversationLocked()); got != 40 {
		t.Errorf("%d of 40 turns left after failed folds", got)
	}
	if n := s.TrimmedMessages() + s.FoldedMessages(); n != 0 {
		t.Errorf("%d messages removed although no summary exists", n)
	}
}

// With no summariser at all (a session built by hand), nothing is ever removed below the ceiling —
// including by the message-count cap, which used to slice the oldest messages off.
func TestNoSummarizerMeansNothingIsDropped(t *testing.T) {
	s := NewConversationSession("bare")
	s.MaxMessages = 5
	s.MaxContextTokens = 50
	s.AddMessage("system", "SYSTEM")
	for i := 0; i < 60; i++ {
		s.AddMessage("user", fmt.Sprintf("m%d", i))
	}
	if got := len(s.GetContextCopy()); got != 61 {
		t.Errorf("context has %d messages, want all 61", got)
	}
}

// A fold that raced a change to the turns it read must not remove anything.
func TestFoldRefusesWhenItsTurnsChanged(t *testing.T) {
	sum := &recordingSummarizer{block: make(chan struct{})}
	s := NewConversationSession("race")
	s.MaxMessages = 1000
	s.MaxContextTokens = 40
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM")
	for i := 0; i < 12; i++ {
		s.AddMessage("user", fmt.Sprintf("sentence %d with enough words to matter", i))
	}
	waitFor(t, "a fold to start", func() bool {
		sum.mu.Lock()
		defer sum.mu.Unlock()
		return sum.calls > 0
	})
	s.mu.Lock()
	s.Context[1].Content = "revised while the fold ran"
	s.mu.Unlock()
	close(sum.block)
	waitIdle(s)

	if s.FoldedMessages() != 0 {
		t.Fatal("a fold was applied over turns that had changed")
	}
	if !strings.Contains(contextText(s), "revised while the fold ran") {
		t.Error("the revised turn was lost")
	}
}

// The knowledge passage and the summary are pinned: never folded, never counted.
func TestPinnedMessagesAreNeverFolded(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("pinned")
	s.MaxMessages = 1000
	s.MaxContextTokens = 60
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM")
	s.SetKnowledgeContext(knowledgeContextPrefix + "]\n" + strings.Repeat("k", 2000))
	for i := 0; i < 30; i++ {
		s.AddMessage("user", fmt.Sprintf("sentence %d with a few words", i))
		waitIdle(s)
	}
	knowledge := 0
	for _, m := range s.GetContextCopy() {
		if strings.HasPrefix(m.Content, knowledgeContextPrefix) {
			knowledge++
		}
	}
	if knowledge != 1 {
		t.Errorf("%d knowledge passages in context, want 1", knowledge)
	}
	if s.FoldedMessages() == 0 {
		t.Error("the conversation never folded although the passage alone is over budget — it must not count")
	}
}

// A tool result is never separated from the call that asked for it.
func TestFoldPointKeepsToolCallsWithTheirResults(t *testing.T) {
	call := []map[string]string{{"id": "c1"}}
	conv := []Message{
		{Role: "user", Content: strings.Repeat("u", 400)},
		{Role: "assistant", ToolCalls: call},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("r", 400)},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "ok"},
	}
	k := foldPoint(conv, 100, 0)
	if k > 0 && (conv[k].Role == "tool" || conv[k-1].ToolCalls != nil) {
		t.Fatalf("fold of %d splits a tool call from its result", k)
	}
	if k < 1 {
		t.Fatalf("fold of %d took nothing, want at least the first turn", k)
	}
}

// The only unsummarised drop, past the ceiling, is counted.
func TestHardCeilingIsTheOnlyDrop(t *testing.T) {
	s := NewConversationSession("huge")
	s.MaxMessages = 0
	s.MaxContextTokens = 0
	s.AddMessage("system", "SYSTEM")
	chunk := strings.Repeat("x", 4000) // 1,000 estimated tokens
	for i := 0; i < 60; i++ {
		s.AddMessage("user", chunk)
	}
	if s.TrimmedMessages() == 0 {
		t.Fatal("60,000 tokens of conversation and nothing dropped: the model request would fail")
	}
	if got := s.ConversationTokens(); got > hardHistoryCeilingTokens {
		t.Errorf("conversation is %d tokens, above the %d ceiling", got, hardHistoryCeilingTokens)
	}
	if s.GetContextCopy()[0].Content != "SYSTEM" {
		t.Error("the system prompt went with it")
	}
}

// The summariser is shown who said what, the record so far, and the rule that nothing concrete may
// be left out.
func TestSummarizeHistoryPrompt(t *testing.T) {
	llm := &capturingLLM{reply: "Caller: Dani."}
	o := NewWithLogger(&MockSTTProvider{}, llm, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	out, err := o.summarizeHistory(context.Background(), "Caller wants a room.", []Message{
		{Role: "user", Content: "soy Dani"},
		{Role: "assistant", Content: "Encantado, Dani."},
	})
	if err != nil || out != "Caller: Dani." {
		t.Fatalf("summarizeHistory = %q, %v", out, err)
	}
	prompt := llm.last[0].Content + "\n" + llm.last[1].Content
	for _, want := range []string{"Caller wants a room.", "Caller: soy Dani", "Agent: Encantado, Dani.", "exactly as it was said", "never drop a detail"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("summariser prompt lacks %q", want)
		}
	}
}

// Every session the Orchestrator builds folds with its model.
func TestSessionsFromTheOrchestratorFold(t *testing.T) {
	llm := &capturingLLM{reply: "record"}
	o := NewWithLogger(&MockSTTProvider{}, llm, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	s := o.NewSessionWithDefaults("call")
	s.MaxContextTokens = 50
	s.AddMessage("system", "SYSTEM")
	for i := 0; i < 20; i++ {
		s.AddMessage("user", fmt.Sprintf("sentence %d with a few words in it", i))
		waitIdle(s)
	}
	if s.FoldedMessages() == 0 {
		t.Fatal("a session from NewSessionWithDefaults never folded")
	}
	if err := o.SummarizeContext(context.Background(), s); err != nil {
		t.Fatalf("SummarizeContext: %v", err)
	}
}

// The default is the conversation budget alone, sized in context_budget.go against the pricing
// model; changing it changes finance/pricing_model.py LLM_CAP.
func TestDefaultBudgetIsTheConversationAlone(t *testing.T) {
	if DefaultMaxContextTokens != 800 {
		t.Errorf("DefaultMaxContextTokens = %d: re-derive LLM_CAP in finance/pricing_model.py (and the worst-case pricing test) before changing it", DefaultMaxContextTokens)
	}
}

type capturingLLM struct {
	mu    sync.Mutex
	reply string
	last  []Message
}

func (c *capturingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = append([]Message(nil), messages...)
	return c.reply, nil
}

func (c *capturingLLM) Name() string { return "capturing" }

// 2026-09-24, first deploy of the fold: the web path sets the agent's prompt, then a client's
// "prompt" message set another, and SetSystemPrompt APPENDED it as a second system message. Only
// index 0 was pinned, so the client's ~2,000-token prompt counted as conversation and was folded
// into a 3-token summary about a second into every web session, before the caller said anything.
func TestAClientPromptIsNeverFolded(t *testing.T) {
	llm := &capturingLLM{reply: "x"}
	o := NewWithLogger(&MockSTTProvider{}, llm, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	s := o.NewSessionWithDefaults("web")
	o.SetSystemPrompt(s, "You are a warm, natural conversational partner.")
	client := "CLIENT PROMPT: " + strings.Repeat("you are the reception desk of Hotel Sol. ", 200)
	o.SetSystemPrompt(s, client)
	s.AddMessage("user", OpeningTrigger)
	s.AddMessage("assistant", "¡Hola! Hotel Sol, ¿en qué puedo ayudarte?")
	waitIdle(s)

	if s.FoldedMessages() != 0 {
		t.Fatalf("%d messages folded before the caller spoke", s.FoldedMessages())
	}
	ctx := s.GetContextCopy()
	systems := 0
	for _, m := range ctx {
		if m.Role == "system" {
			systems++
		}
	}
	if systems != 1 {
		t.Errorf("%d system messages, want the one prompt SetSystemPrompt was last given", systems)
	}
	if !strings.Contains(ctx[0].Content, "CLIENT PROMPT") {
		t.Error("the client's prompt is not the system prompt")
	}
	if strings.Contains(ctx[0].Content, "warm, natural conversational partner") {
		t.Error("the replaced prompt is still there")
	}
}

// A system message anywhere is instructions, not conversation: never counted, never folded.
func TestStraySystemMessagesArePinned(t *testing.T) {
	sum := &recordingSummarizer{}
	s := NewConversationSession("stray")
	s.MaxMessages = 1000
	s.MaxContextTokens = 50
	s.SetHistorySummarizer(sum.summarize, nil)
	s.AddMessage("system", "SYSTEM")
	s.AddMessage("user", "hola")
	s.AddMessage("system", "EXTRA INSTRUCTIONS "+strings.Repeat("z", 2000))
	s.AddMessage("assistant", "hola")
	waitIdle(s)
	if sum.calls != 0 {
		t.Errorf("a fold ran for a 2-message conversation because a system message was counted")
	}
	if !strings.Contains(contextText(s), "EXTRA INSTRUCTIONS") {
		t.Error("the extra instructions were folded away")
	}
}

// SetSystemPrompt on a session whose first message is the call summary puts the prompt first.
func TestSetSystemPromptGoesFirst(t *testing.T) {
	o := NewWithLogger(&MockSTTProvider{}, &capturingLLM{}, &MockTTSProvider{}, nil, DefaultConfig(), &NoOpLogger{})
	s := NewConversationSession("s")
	s.Context = []Message{{Role: "system", Content: callSummaryHeader + "Caller: Dani."}, {Role: "user", Content: "hola"}}
	o.SetSystemPrompt(s, "PROMPT")
	ctx := s.GetContextCopy()
	if !strings.Contains(ctx[0].Content, "PROMPT") || !strings.HasPrefix(ctx[1].Content, callSummaryPrefix) {
		t.Fatalf("got %q then %q", ctx[0].Content[:10], ctx[1].Content[:10])
	}
}
