package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_PlaybackAlignedEchoDetection(t *testing.T) {
	orch := New(nil, nil, nil, nil, Config{}, nil)
	sess := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, sess)

	ms.vad = NewRMSVAD(0.02, 50*time.Millisecond)

	played := make([]byte, 4410*2) 
	for i := 0; i < len(played)-1; i += 2 {
		val := int16(8000)
		played[i] = byte(val)
		played[i+1] = byte(val >> 8)
	}

	ms.RecordPlayedOutput(played)

	err := ms.Write(played)
	if err != nil {
		t.Fatal(err)
	}

	chunk := make([]byte, 1024)
	for i := 0; i < len(chunk)-1; i += 2 {
		val := int16(8000)
		chunk[i] = byte(val)
		chunk[i+1] = byte(val >> 8)
	}

	err = ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	if ms.IsUserSpeaking() {
		t.Fatal("expected echo to be suppressed and not mark user as speaking")
	}
}
