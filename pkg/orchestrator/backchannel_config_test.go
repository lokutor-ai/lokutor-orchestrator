package orchestrator

import (
	"context"
	"testing"
	"time"
)

// Backchannels are off unless configured: no detector, and nothing synthesised for them at session
// start. When configured, the detector reads the caller's audio at its real 16 kHz, not the
// playback rate — at 44100 every pitch it measured was 2.76x too high.
func TestBackchannelsOffByDefault(t *testing.T) {
	tts := &MockTTSProvider{synthesizeResult: []byte("audio")}
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hi"}, &MockLLMProvider{completeResult: "ok"}, tts,
		NewRMSVAD(0.05, 50*time.Millisecond), DefaultConfig())
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("bc-off"))
	defer stream.Close()
	if stream.backch != nil {
		t.Fatal("a stream built with the default config must have no backchannel detector")
	}
	stream.RegenerateBackchannelClips(orch) // must be a no-op without a detector
}

func TestBackchannelsWhenConfiguredReadCallerAudioAt16k(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backchannels = true
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hi"}, &MockLLMProvider{completeResult: "ok"},
		&MockTTSProvider{synthesizeResult: []byte("audio")}, NewRMSVAD(0.05, 50*time.Millisecond), cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("bc-on"))
	defer stream.Close()
	if stream.backch == nil {
		t.Fatal("Backchannels=true must build the detector")
	}
	if stream.backch.sampleRate != 16000 {
		t.Fatalf("detector sample rate %d, want 16000 (the caller's audio)", stream.backch.sampleRate)
	}
}
