package orchestrator

import (
	"context"
	"testing"
	"time"
)

// On hold (a transfer ringing) the caller's audio never reaches the VAD, and the chunks dropped are
// counted, so the hold leaves a trace in the log.
func TestHoldDropsCallerAudioAndCountsIt(t *testing.T) {
	log := &replayLog{}
	ms := &ManagedStream{logger: log, cmdChan: make(chan []byte, 4)}
	ms.Hold(true)
	for i := 0; i < 3; i++ {
		_ = ms.Write([]byte{1, 2})
	}
	if len(ms.cmdChan) != 0 {
		t.Fatalf("audio reached the stream while on hold: %d chunks", len(ms.cmdChan))
	}
	if !ms.IsHeld() {
		t.Fatal("IsHeld is false while held")
	}
	ms.Hold(false)
	got := log.find("Hold ended: caller audio dropped while on hold")
	if len(got) != 1 || got[0].kv["chunks"] != int64(3) {
		t.Fatalf("want one log line with 3 chunks, got %+v", got)
	}
	_ = ms.Write([]byte{1, 2})
	if len(ms.cmdChan) != 1 {
		t.Fatalf("audio after the hold did not reach the stream")
	}
}

// A host note reaches the model as the conversation plus the instruction for this one request; the note
// itself is kept in the conversation (later turns must know the transfer failed), the instruction is not.
func TestHostNoteMessages(t *testing.T) {
	session := NewConversationSession("u")
	session.AddMessage("user", "Quiero hablar con una persona.")
	session.AddMessage("assistant", "Le paso con recepción, un momento.")
	ms := &ManagedStream{session: session, logger: &replayLog{}}
	ms.ctx, ms.cancel = context.WithCancel(context.Background())
	ms.cancel() // closed: HostNote stores the note, then does nothing else
	if ms.HostNote("x", "y", time.Millisecond) != nil {
		t.Fatal("HostNote on a closed stream must return nil")
	}

	ms.session.AddMessage("system", "[The transfer to Recepción was not answered.]")
	ms.hostNoteInstruction = "Tell the caller nobody answered and offer to take a message."
	stored := len(ms.session.GetContextCopy())
	got := ms.llmMessages(hostNoteTrigger)
	if len(got) != stored+1 {
		t.Fatalf("want the conversation plus the instruction, got %d for %d stored", len(got), stored)
	}
	if last := got[len(got)-1]; last.Role != "user" || last.Content != ms.hostNoteInstruction {
		t.Fatalf("instruction = %+v", last)
	}
	if n := len(ms.session.GetContextCopy()); n != stored {
		t.Fatalf("the instruction was stored in the conversation (%d, want %d)", n, stored)
	}
	if responseTriggerFor(hostNoteTrigger) != "host_note" {
		t.Fatal("a host note must be logged as a bot-initiated response")
	}
	if tools := ms.toolsOffered(hostNoteTrigger); tools == nil && len(ms.session.GetTools()) > 0 {
		t.Fatal("a host note must keep the agent's tools: it may need to take a message or end the call")
	}
}

// A host note is news, never a cached reply.
func TestHostNoteSkipsTheResponseCache(t *testing.T) {
	ms := &ManagedStream{responseCache: NewResponseCache(time.Minute, 16), logger: &replayLog{}}
	ms.cacheResponse(hostNoteTrigger, "cached", nil)
	if _, _, ok := ms.checkResponseCache(hostNoteTrigger); ok {
		t.Fatal("a host note was answered from the cache")
	}
}
