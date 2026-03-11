package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type MockStreamingLLM struct {
	responses []struct {
		content   string
		toolCalls []ToolCallEventData
	}
	callCount int
}

func (m *MockStreamingLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	if m.callCount >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return resp.content, nil
}

func (m *MockStreamingLLM) StreamComplete(ctx context.Context, messages []Message, tools []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	if m.callCount >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callCount]
	m.callCount++

	if resp.content != "" {
		if onChunk != nil {
			// Simulate streaming with turn token
			onChunk(string(TurnComplete) + " ")
			onChunk(resp.content)
		}
	}

	for _, tc := range resp.toolCalls {
		if onToolCall != nil {
			onToolCall(tc)
		}
	}

	return resp.content, nil
}

func (m *MockStreamingLLM) Name() string { return "MockStreamingLLM" }

func TestManagedStream_ToolCalling(t *testing.T) {
	llm := &MockStreamingLLM{
		responses: []struct {
			content   string
			toolCalls []ToolCallEventData
		}{
			{
				content: "",
				toolCalls: []ToolCallEventData{
					{Name: "get_weather", Arguments: `{"location":"Madrid"}`, CallID: "c1"},
				},
			},
			{
				content:   "The weather in Madrid is sunny.",
				toolCalls: nil,
			},
		},
	}

	stt := &MockSTTProvider{transcribeResult: "whats the weather?"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewMockProsodyAnalyzer()

	orch := NewWithAllLayers(stt, llm, tts, nil, vad, DefaultConfig(), &NoOpLogger{})

	weatherCalled := false
	orch.RegisterTool("get_weather", func(args string) (string, error) {
		weatherCalled = true
		var params struct{ Location string }
		json.Unmarshal([]byte(args), &params)
		return fmt.Sprintf("It is currently sunny in %s", params.Location), nil
	})

	session := NewConversationSession("test_user")
	ms := orch.NewManagedStream(context.Background(), session)
	defer ms.Close()

	// Trigger response
	go ms.runLLMAndTTS(context.Background(), "whats the weather?")

	// We expect multiple events: BotThinking, ToolCall, BotThinking (recursive), BotResponse, BotSpeaking
	timeout := time.After(2 * time.Second)
	var events []EventType
loop:
	for {
		select {
		case ev := <-ms.Events():
			events = append(events, ev.Type)
			if ev.Type == BotSpeaking {
				break loop
			}
		case <-timeout:
			t.Fatalf("Timed out waiting for events. Got: %v", events)
		}
	}

	if !weatherCalled {
		t.Error("get_weather tool was never called")
	}

	// Verify conversation history has tool result
	ctx := session.GetContextCopy()
	hasToolMsg := false
	for _, m := range ctx {
		if m.Role == "tool" {
			hasToolMsg = true
			if !strings.Contains(m.Content, "sunny") {
				t.Errorf("Unexpected tool result: %s", m.Content)
			}
		}
	}
	if !hasToolMsg {
		t.Error("Tool result message not found in session context")
	}

	foundFinalResponse := false
	for _, m := range ctx {
		if m.Role == "assistant" && strings.Contains(m.Content, "weather in Madrid is sunny") {
			foundFinalResponse = true
		}
	}
	if !foundFinalResponse {
		t.Error("Final assistant response not found in session context")
	}
}
