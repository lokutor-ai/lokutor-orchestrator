package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TurnState represents the decision from the LLM about turn completion
type TurnState string

const (
	TurnComplete TurnState = "✓"
	TurnWait5    TurnState = "○"
	TurnWait10   TurnState = "◐"
)

// Layer2ProsodyAnalyzer analyzes raw waveform to predict if a sentence is complete based on intonation
type Layer2ProsodyAnalyzer interface {
	// IsSentenceComplete returns the probability (0.0 to 1.0) that the user has finished speaking
	IsSentenceComplete(audio []byte) float64
}

// Layer3LLMAnalyzer parses the LLM output for turn completion tokens
type Layer3LLMAnalyzer struct {
	llm LLMProvider
}

func NewLayer3LLMAnalyzer(llm LLMProvider) *Layer3LLMAnalyzer {
	return &Layer3LLMAnalyzer{llm: llm}
}

// EvaluateContext prepends a system prompt instructing the LLM to output a turn token
func (l *Layer3LLMAnalyzer) EvaluateContext(ctx context.Context, session *ConversationSession) (TurnState, string, error) {
	// Copy context and inject the turn-taking instruction
	messages := session.GetContextCopy()
	
	systemInstruction := "You are a conversational AI. Before you formulate your response, you must evaluate if the user has finished their thought. " +
		"Output EXACTLY ONE of the following tokens at the very beginning of your response:\n" +
		"✓ : Respond now (The user provided a complete thought, answered a question, or gave a short practical instruction like 'Continúa' or 'Ayúdame').\n" +
		"○ : Wait 5 seconds (The user objectively stopped mid-sentence with no semantic conclusion, e.g. 'Quiero que...')\n" +
		"◐ : Wait 10 seconds (The user explicitly asked to wait, e.g. 'Espera un momento').\n" +
		"CRITICAL: Be extremely stingy with ○ and ◐. Always default to ✓ unless the thought is structurally broken. Short responses like 'No sé', 'No', 'Sí', 'Continúa' are ALWAYS ✓. Respond immediately to them." +
		"After the token, if you chose ✓, output your response. If you chose ○ or ◐, you may output nothing else."
	
	// Prepend system message
	messages = append([]Message{{Role: "system", Content: systemInstruction}}, messages...)

	resp, err := l.llm.Complete(ctx, messages, session.GetTools())
	if err != nil {
		return TurnComplete, "", err
	}

fmt.Printf("\n[LAYER 3 DEBUG]: %s\n", resp)

	resp = strings.TrimSpace(resp)
	if strings.HasPrefix(resp, string(TurnComplete)) {
		return TurnComplete, strings.TrimSpace(strings.TrimPrefix(resp, string(TurnComplete))), nil
	} else if strings.HasPrefix(resp, string(TurnWait5)) {
		return TurnWait5, "", nil
	} else if strings.HasPrefix(resp, string(TurnWait10)) {
		return TurnWait10, "", nil
	}

	return TurnComplete, resp, nil
}

// MockProsodyAnalyzer is a placeholder for the 12ms CPU model
type MockProsodyAnalyzer struct {
	threshold float64
}

func NewMockProsodyAnalyzer() *MockProsodyAnalyzer {
	return &MockProsodyAnalyzer{threshold: 0.5}
}

func (m *MockProsodyAnalyzer) IsSentenceComplete(audio []byte) float64 {
	// Mock implementation
	return 0.8
}

// Layer1SileroVAD is a placeholder for a Silero VAD implementation
type Layer1SileroVAD struct {
	*RMSVAD 
}

func NewLayer1SileroVAD(threshold float64, silenceLimit time.Duration) *Layer1SileroVAD {
	return &Layer1SileroVAD{
		RMSVAD: NewRMSVAD(threshold, silenceLimit),
	}
}

func (s *Layer1SileroVAD) Name() string {
	return "silero_vad"
}
