package orchestrator

import "testing"

func TestMessage(t *testing.T) {
	msg := Message{Role: "user", Content: "Hello"}
	if msg.Role != "user" {
		t.Errorf("Expected role 'user', got '%s'", msg.Role)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SampleRate != 16000 {
		t.Errorf("Expected sample rate 16000, got %d", cfg.SampleRate)
	}
	if cfg.MaxContextMessages != 20 {
		t.Errorf("Expected max messages 20, got %d", cfg.MaxContextMessages)
	}
}

func TestNewConversationSession(t *testing.T) {
	session := NewConversationSession("user_123")
	if session.ID != "user_123" {
		t.Errorf("Expected ID 'user_123', got '%s'", session.ID)
	}
	if len(session.Context) != 0 {
		t.Errorf("Expected empty context")
	}
}

func TestAddMessage(t *testing.T) {
	session := NewConversationSession("user_456")
	session.AddMessage("user", "Hello")
	if len(session.Context) != 1 {
		t.Errorf("Expected 1 message")
	}
	if session.LastUser != "Hello" {
		t.Errorf("Expected last user 'Hello'")
	}
}

func TestClearContext(t *testing.T) {
	session := NewConversationSession("user_789")
	session.AddMessage("user", "Test")
	session.ClearContext()
	if len(session.Context) != 0 {
		t.Errorf("Expected empty context after clear")
	}
}
