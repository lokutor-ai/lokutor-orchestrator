package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundLLM answers Complete (the speculative run) with words and a tool call through the capture, and StreamComplete
// (the real turn) from a script, counting both.
type roundLLM struct {
	mu        sync.Mutex
	completes int
	streams   int
	script    []string
}

func (m *roundLLM) Name() string { return "roundLLM" }

func (m *roundLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	m.mu.Lock()
	m.completes++
	m.mu.Unlock()
	ToolCallCaptureFrom(ctx).Add(ToolCallEventData{Name: "lookup", Arguments: `{}`, CallID: "c1"})
	return "Un momento, lo miro.", nil
}

func (m *roundLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	m.mu.Lock()
	i := m.streams
	m.streams++
	m.mu.Unlock()
	if i >= len(m.script) {
		return "", nil
	}
	if err := onChunk(m.script[i]); err != nil {
		return "", err
	}
	return m.script[i], nil
}

// A speculative reply that called a tool is a round, not a reply: Await reads it as no reply, AwaitRound returns the
// words and the call.
func TestSpeculativeToolRoundIsKept(t *testing.T) {
	llm := &roundLLM{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, &countingTTS{}, nil, DefaultConfig(), &NoOpLogger{})
	se := NewSpeculativeExecutor(0)
	se.StartFromTranscript(context.Background(), orch, "mira si hay hueco el lunes", nil, nil)
	text, calls, ok := se.AwaitRound(context.Background(), "Mira si hay hueco el lunes.")
	if !ok || text != "Un momento, lo miro." || len(calls) != 1 || calls[0].Name != "lookup" {
		t.Fatalf("AwaitRound: %q %+v %v", text, calls, ok)
	}
	if r, ok := se.Await(context.Background(), "Mira si hay hueco el lunes."); ok || r != "" {
		t.Fatalf("Await took a tool round as a reply: %q", r)
	}
}

// The turn replays the round: the words are said once, the tool runs once, and the model is asked only for the round
// after the tool, not the first one again.
func TestTurnReplaysTheSpeculativeToolRound(t *testing.T) {
	llm := &roundLLM{script: []string{"El lunes a las diez está libre."}}
	tts := &countingTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	var ran int
	var mu sync.Mutex
	orch.RegisterTool("lookup", func(args string) (string, error) { mu.Lock(); ran++; mu.Unlock(); return `{"free":["10:00"]}`, nil })
	session := NewConversationSession("spec-round")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "lookup"}}})
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	ms.specToolRound = &specToolRound{transcript: "mira si hay hueco el lunes", text: "Un momento, lo miro.",
		calls: []ToolCallEventData{{Name: "lookup", Arguments: `{}`, CallID: "c1"}}}
	ms.runLLMAndTTS(context.Background(), "mira si hay hueco el lunes")

	deadline := time.Now().Add(3 * time.Second)
	for {
		tts.mu.Lock()
		joined := strings.Join(tts.said, " | ")
		tts.mu.Unlock()
		if strings.Contains(joined, "diez") || time.Now().After(deadline) {
			if n := strings.Count(joined, "lo miro"); n != 1 {
				t.Errorf("the words before the call were said %d times: %s", n, joined)
			}
			if !strings.Contains(joined, "diez") {
				t.Errorf("the reply after the tool was not said: %s", joined)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if ran != 1 {
		t.Errorf("the tool ran %d times", ran)
	}
	if llm.streams != 1 || llm.completes != 0 {
		t.Errorf("model asked %d streamed / %d whole times, want 1 / 0 (only the round after the tool)", llm.streams, llm.completes)
	}
}

// countingRAG answers every query with one passage, or fails, counting calls.
type countingRAG struct {
	mu      sync.Mutex
	calls   []string
	content string
	err     error
}

func (r *countingRAG) Retrieve(ctx context.Context, query string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, query)
	return r.content, r.err
}

// The seed is asked with the passage its turn will have, arranged as injectRagContext arranges it, and the turn
// reuses that retrieval instead of fetching again; a guess made with any other passage is not used.
func TestSpeculativeSeedCarriesThisTurnsKnowledge(t *testing.T) {
	rag := &countingRAG{content: "Abrimos de 9 a 18."}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, &roundLLM{}, &countingTTS{}, nil, DefaultConfig(), &NoOpLogger{})
	orch.SetRAGProvider(rag)
	session := NewConversationSession("rag-seed")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	old := Message{Role: "system", Content: knowledgeMessage("Aparcamiento gratuito.")}
	history := []Message{{Role: "system", Content: "Eres Sofía."}, {Role: "user", Content: "¿Hay parking?"}, old, {Role: "assistant", Content: "Sí."}}
	kept, tail := ms.seedKnowledgeRequest(context.Background(), "¿a qué hora abrís?", history)
	if len(kept) != 3 || kept[2].Content != "Sí." || len(tail) != 1 || tail[0].Content != knowledgeMessage("Abrimos de 9 a 18.") {
		t.Fatalf("history %+v tail %+v", kept, tail)
	}

	// The turn on the same words reuses it: no second retrieval, and a guess may be used.
	ms.injectRagContext(context.Background(), "¿A qué hora abrís?")
	rag.mu.Lock()
	n := len(rag.calls)
	rag.mu.Unlock()
	if n != 1 || !ms.knowledgeFromSeed {
		t.Fatalf("retrievals %d, fromSeed %v", n, ms.knowledgeFromSeed)
	}
	ctxMsgs := session.GetContextCopy()
	if last := ctxMsgs[len(ctxMsgs)-1]; last.Content != knowledgeMessage("Abrimos de 9 a 18.") {
		t.Fatalf("knowledge not set: %+v", last)
	}

	// Other words: retrieved again, and no guess is used.
	ms.injectRagContext(context.Background(), "¿y los sábados?")
	if len(rag.calls) != 2 || ms.knowledgeFromSeed {
		t.Fatalf("retrievals %d, fromSeed %v", len(rag.calls), ms.knowledgeFromSeed)
	}
	if ms.trySpeculativeResponse(context.Background(), "¿y los sábados?") {
		t.Fatal("a guess made with another passage was used")
	}

	// Nothing matched: the conversation is left as it is (the turn keeps its earlier passage too).
	rag.content = ""
	kept, tail = ms.seedKnowledgeRequest(context.Background(), "gracias", history)
	if len(kept) != len(history) || tail != nil {
		t.Fatalf("nothing matched changed the request: %+v %+v", kept, tail)
	}
}
