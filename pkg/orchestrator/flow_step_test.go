package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// stepLLM is one step's model: it records what it was asked with and either calls a tool or speaks.
type stepLLM struct {
	name  string
	call  string // tool to call once
	say   string
	mu    sync.Mutex
	asked int
	seen  []string // the system prompt of each request
	tools [][]string
}

func (s *stepLLM) Name() string { return s.name }
func (s *stepLLM) Complete(ctx context.Context, m []Message, t []Tool) (string, error) {
	return s.say, nil
}
func (s *stepLLM) StreamComplete(ctx context.Context, m []Message, t []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	s.mu.Lock()
	s.asked++
	n := s.asked
	for _, x := range m {
		if x.Role == "system" {
			s.seen = append(s.seen, x.Content)
			break
		}
	}
	var names []string
	for _, x := range t {
		if fn, ok := x.Function.(map[string]interface{}); ok {
			names = append(names, fn["name"].(string))
		}
	}
	s.tools = append(s.tools, names)
	s.mu.Unlock()
	if s.call != "" && n == 1 {
		return "", onToolCall(ToolCallEventData{Name: s.call, Arguments: "{}", CallID: "c1"})
	}
	return s.say, onChunk(s.say)
}

func toolNamed(n string) Tool {
	return Tool{Type: "function", Function: map[string]interface{}{"name": n, "description": n,
		"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}}
}

// A tool handler may move the conversation to a step with its own model, prompt and tools: the reply after the
// tool comes from the new step's model, with its prompt and tools, and no filler is spoken for the move.
func TestToolMovesConversationToAnotherModel(t *testing.T) {
	a := &stepLLM{name: "a", call: "go_to_billing"}
	b := &stepLLM{name: "b", say: "You are speaking with billing now."}
	tts := &recordingTTS{}
	orch := NewWithVAD(&MockSTTProvider{}, a, tts, &bcVAD{}, DefaultConfig())
	session := orch.NewSessionWithDefaults("s")
	orch.SetSystemPrompt(session, "STEP ONE")
	session.SetTools([]Tool{toolNamed("go_to_billing")})
	orch.SetQuietTools("go_to_billing")
	orch.RegisterTool("go_to_billing", func(string) (string, error) {
		orch.SetLLMProvider(b)
		orch.SetSystemPrompt(session, "STEP TWO")
		session.SetTools([]Tool{toolNamed("pay_invoice")})
		return `{"ok":true}`, nil
	})
	ms := NewManagedStream(context.Background(), orch, session)
	defer ms.Close()
	go func() {
		for range ms.Events() {
		}
	}()
	session.AddMessage("user", "I have a question about my invoice")
	ms.mu.Lock()
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()
	ms.runStreamingLLM(context.Background(), a, gen, "I have a question about my invoice")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(tts.all(), "billing") {
		time.Sleep(20 * time.Millisecond)
	}
	if got := tts.all(); !strings.Contains(got, "You are speaking with billing now.") {
		t.Fatalf("spoken %q", got)
	}
	if got := tts.all(); strings.Count(got, "|") != 0 {
		t.Fatalf("a filler was spoken for a quiet tool: %q", got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.asked != 1 || !strings.Contains(b.seen[0], "STEP TWO") || len(b.tools[0]) != 1 || b.tools[0][0] != "pay_invoice" {
		t.Fatalf("second step asked %d times with prompt has STEP TWO=%v tools=%v", b.asked, strings.Contains(strings.Join(b.seen, ""), "STEP TWO"), b.tools)
	}
}

type recordingTTS struct {
	mu    sync.Mutex
	texts []string
}

func (r *recordingTTS) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.texts, "|")
}
func (r *recordingTTS) Name() string { return "recording" }
func (r *recordingTTS) Abort() error { return nil }
func (r *recordingTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return make([]byte, 320), nil
}
func (r *recordingTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
	return onChunk(make([]byte, 320))
}

func TestFinalToolResultCanContinue(t *testing.T) {
	o := NewWithVAD(&MockSTTProvider{}, &stepLLM{}, &recordingTTS{}, &bcVAD{}, DefaultConfig())
	o.SetFinalTools("save")
	ex := func(res string) []toolExchange {
		return []toolExchange{{TC: ToolCallEventData{Name: "save"}, Result: res}}
	}
	if !o.allFinalTools(ex(`{"ok":true}`)) || !o.allFinalTools(ex(`{"continue":false}`)) || !o.allFinalTools(ex("not json continue")) {
		t.Error("a plain result must end the turn")
	}
	if o.allFinalTools(ex(`{"ok":true,"continue":true}`)) {
		t.Error("a result that moves on must be answered")
	}
}
