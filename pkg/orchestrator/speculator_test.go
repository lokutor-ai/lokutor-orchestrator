package orchestrator

import (
	"context"
	"testing"
	"time"
)

type mockSpeculativeProvider struct {
	response string
	err      error
}

func (m *mockSpeculativeProvider) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	return m.response, m.err
}

func (m *mockSpeculativeProvider) Name() string {
	return "mock-speculative"
}

func TestSpeculatorDisabled(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test-user")
	sp := NewSpeculator(orch, session, false)

	candidate := sp.OnInterimTranscript(context.Background(), "hello there")
	if candidate != nil {
		t.Error("Expected nil candidate when disabled")
	}
}

func TestSpeculatorNoProvider(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test-user")
	sp := NewSpeculator(orch, session, true)

	candidate := sp.OnInterimTranscript(context.Background(), "hello there")
	if candidate != nil {
		t.Error("Expected nil candidate when no speculative provider")
	}
}

func TestSpeculatorTooShort(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test-user")
	sp := NewSpeculator(orch, session, true)
	sp.SetSpeculativeProvider(&mockSpeculativeProvider{response: "test response"})

	// Only 1 word, needs at least `thinkingWords` (default 3)
	candidate := sp.OnInterimTranscript(context.Background(), "hi")
	if candidate != nil {
		t.Error("Expected nil candidate for very short transcript")
	}
}

func TestSpeculatorFullFlow(t *testing.T) {
	orch := &Orchestrator{
		llm: &mockSpeculativeProvider{
			response: "I think the weather is nice today. It's warm and sunny.",
		},
	}
	session := NewConversationSession("test-user")
	session.AddMessage("user", "previous message")
	session.AddMessage("assistant", "previous response")

	sp := NewSpeculator(orch, session, true)
	sp.SetSpeculativeProvider(&mockSpeculativeProvider{
		response: "I think the weather is nice today. It's warm and sunny.",
	})

	// First interim transcript triggers speculation
	candidate := sp.OnInterimTranscript(context.Background(), "what do you think about the")
	if candidate == nil {
		t.Fatal("Expected non-nil candidate")
	}
	if candidate.state != SpecWorking {
		t.Errorf("Expected SpecWorking state, got %v", candidate.state)
	}

	// Wait for speculation goroutine to complete (mock is synchronous)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		candidate.mu.Lock()
		state := candidate.state
		candidate.mu.Unlock()
		if state != SpecWorking {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	candidate.mu.Lock()
	state := candidate.state
	candidate.mu.Unlock()

	if state != SpecReady && state != SpecFailed {
		t.Errorf("Expected SpecReady or SpecFailed, got %v (confidence=%.2f)", state, candidate.confidence)
	}

	// On final transcript, should accept if similar enough
	result := sp.OnFinalTranscript(context.Background(), "what do you think about the weather")
	if result != nil && result.acceptOnFinal {
		sentence, audio := sp.AcceptAndConsume()
		if sentence == "" {
			t.Error("Expected non-empty sentence")
		}
		if audio != nil {
			t.Logf("Got pre-synthesized audio of %d bytes", len(audio))
		}
	}
}

func TestSpeculatorReset(t *testing.T) {
	orch := &Orchestrator{
		llm: &mockSpeculativeProvider{response: "test"},
	}
	session := NewConversationSession("test-user")
	sp := NewSpeculator(orch, session, true)
	sp.SetSpeculativeProvider(&mockSpeculativeProvider{response: "test"})

	sp.OnInterimTranscript(context.Background(), "hello world test")
	sp.Reset()

	if sp.GetState() != SpecIdle {
		t.Errorf("Expected SpecIdle after reset, got %v", sp.GetState())
	}
}

func TestSpeculateConfidence(t *testing.T) {
	tests := []struct {
		name       string
		partial    string
		response   string
		minConfidence float64
	}{
		{
			name:       "high overlap",
			partial:    "what is the weather",
			response:   "The weather today is nice and sunny.",
			minConfidence: 0.5,
		},
		{
			name:       "no overlap",
			partial:    "hello there",
			response:   "The sky is blue.",
			minConfidence: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := speculateConfidence(tt.partial, tt.response)
			if c < tt.minConfidence {
				t.Errorf("speculateConfidence(%q, %q) = %.2f, want >= %.2f",
					tt.partial, tt.response, c, tt.minConfidence)
			}
		})
	}
}

func TestSpecSimilarity(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want float64
	}{
		{"hello world", "hello world", 1.0},
		{"hello", "world", 0.0},
		{"hello world", "hello", 0.5}, // hello is in both, world is not in b
		{"", "hello", 0.0},
	}

	for _, tt := range tests {
		got := specSimilarity(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("specSimilarity(%q, %q) = %.2f, want %.2f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestExtractFirstSentence(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello world. How are you?", "Hello world."},
		{"Single sentence.", "Single sentence."},
		{"What's up? Not much.", "What's up?"},
		{"Dr. Smith is here.", "Dr. Smith is here."}, // abbreviation should not split
		{"¡Hola! ¿Cómo estás?", "¡Hola!"},
		{"No punctuation", "No punctuation"},
	}

	for _, tt := range tests {
		t.Run(tt.input[:min(len(tt.input), 20)], func(t *testing.T) {
			got := extractFirstSentence(tt.input)
			if got != tt.want {
				t.Errorf("extractFirstSentence(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestWordSet(t *testing.T) {
	set := wordSet("Hello world hello")
	if len(set) != 2 {
		t.Errorf("Expected 2 unique words, got %d", len(set))
	}
	if !set["hello"] {
		t.Error("Expected 'hello' in set (case insensitive)")
	}
	if !set["world"] {
		t.Error("Expected 'world' in set")
	}
}
