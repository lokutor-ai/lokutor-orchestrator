package orchestrator

import (
	"context"
	"sync"
	"testing"
)

func TestSessionMeterAddsUpByCategory(t *testing.T) {
	m := NewSessionMeter()
	m.Add("", 100, 10) // no category is a reply
	m.Add(TokensReply, 50, 5)
	m.Add(TokensSpeculative, 200, 20)
	m.Add(TokensSummary, 0, 0) // a request with no counts records nothing
	got := m.Snapshot()
	if r := got[TokensReply]; r.Prompt != 150 || r.Completion != 15 || r.Requests != 2 {
		t.Fatalf("reply = %+v", r)
	}
	if s := got[TokensSpeculative]; s.Prompt != 200 || s.Completion != 20 || s.Requests != 1 {
		t.Fatalf("speculative = %+v", s)
	}
	if _, ok := got[TokensSummary]; ok {
		t.Fatal("an empty report created a category")
	}
}

func TestSessionMeterIsSafeNilAndConcurrent(t *testing.T) {
	var nilMeter *SessionMeter
	nilMeter.Add(TokensReply, 1, 1) // must not panic
	if len(nilMeter.Snapshot()) != 0 {
		t.Fatal("nil meter has counts")
	}
	m := NewSessionMeter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Add(TokensReply, 10, 1) }()
	}
	wg.Wait()
	if r := m.Snapshot()[TokensReply]; r.Prompt != 500 || r.Requests != 50 {
		t.Fatalf("lost concurrent adds: %+v", r)
	}
}

func TestTokenCategoryTravelsOnTheContext(t *testing.T) {
	ctx := context.Background()
	if TokenCategoryFrom(ctx) != TokensReply {
		t.Fatal("default category is not reply")
	}
	if TokenCategoryFrom(WithTokenCategory(ctx, TokensSpeculative)) != TokensSpeculative {
		t.Fatal("category lost")
	}
	m := NewSessionMeter()
	if SessionMeterFrom(WithTokenCategory(WithSessionMeter(ctx, m), TokensSummary)) != m {
		t.Fatal("meter lost under a category")
	}
}

// The host can install the meter before the stream exists and read the bill from it; a stream built
// without one gets its own.
func TestManagedStreamUsesTheHostsMeter(t *testing.T) {
	orch := NewWithAllLayers(&MockSTTProvider{}, &sequencedLLM{}, &blockingAfterFirstTTS{}, nil, DefaultConfig(), &recordingLogger{})
	m := NewSessionMeter()
	ms := orch.NewManagedStream(WithSessionMeter(context.Background(), m), NewConversationSession("meter"))
	defer ms.Close()
	if SessionMeterFrom(ms.ctx) != m {
		t.Fatal("the stream did not keep the host's meter")
	}
	ms2 := orch.NewManagedStream(context.Background(), NewConversationSession("meter2"))
	defer ms2.Close()
	if SessionMeterFrom(ms2.ctx) == nil {
		t.Fatal("a stream without a host meter has none")
	}
}
