package orchestrator

import (
	"context"
	"testing"
	"time"
)

// Closing a stream must not call the model. Close used to send the call's transcript to the LLM
// for "key facts about this user" and store the answer in session.UserMemory — on a session that
// is discarded as the call ends, so nothing ever read it. Production paid for one extra completion
// on every call, on the same quota the live callers share. What the caller said on earlier calls
// reaches the next call from the stored conversation history instead (lokutor_tts call_context.go).
func TestCloseMakesNoLLMCall(t *testing.T) {
	llm := &countingLLM{result: "Name: Dani"}
	cfg := DefaultConfig()
	cfg.FirstSpeaker = FirstSpeakerUser // no greeting: the model has no reason to be called at all
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hola"}, llm,
		&MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}, NewRMSVAD(0.1, 100*time.Millisecond), cfg)
	session := NewConversationSession("close-no-llm")
	session.AddMessage("user", "Hola, soy Dani y llamo por la cita del jueves.")
	session.AddMessage("assistant", "Hola Dani, ¿a qué hora te viene bien el jueves?")
	session.AddMessage("user", "A las cinco.")

	stream := orch.NewManagedStream(context.Background(), session)
	stream.Close()

	// The extraction ran on its own goroutine, within microseconds of Close; this is ample.
	time.Sleep(300 * time.Millisecond)
	if n := llm.calls.Load(); n != 0 {
		t.Fatalf("Close made %d LLM call(s); it must make none", n)
	}
	session.mu.RLock()
	mem := session.UserMemory
	session.mu.RUnlock()
	if mem != "" {
		t.Fatalf("Close wrote UserMemory %q; nothing reads it after the call", mem)
	}
}
