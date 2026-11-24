package orchestrator

import (
	"context"
	"testing"
	"time"
)

// A match on sound alone may not drop a caller's words (web call, 2026-11-24: 0.7509 over a bar of 0.75 at 420 ms) unless
// the call proved its echo path at that delay: a strong match by words and sound. A weak one proves nothing.
func TestEchoSoundAlone_OnlyAtAProvenDelay(t *testing.T) {
	stream := NewWithVAD(&MockSTTProvider{}, &MockLLMProvider{}, &MockTTSProvider{}, NewRMSVAD(0.05, 50*time.Millisecond),
		DefaultConfig()).NewManagedStream(context.Background(), NewConversationSession("sound-alone"))
	defer stream.Close()
	talk := voiced(syllables(100, 1.1), 0.37)
	far := append(make([]byte, 16000*2), talk...)
	const lag = 8
	lagBytes, nearBytes := lag*echoFrameBytes, 43*echoFrameBytes
	near := attenuate(far[len(far)-lagBytes-nearBytes:len(far)-lagBytes], 0.3)
	set := func() {
		stream.echoMu.Lock()
		stream.echoFarEndBuf, stream.echoNearEndBuf, stream.echoNearEndGen = far, near, 1
		stream.echoMu.Unlock()
	}
	set()
	r := stream.acousticEchoCheck(1)
	if !r.echo || echoSoundAloneVetoes(r) {
		t.Fatalf("no proven delay: the check must match and not veto, got %+v", r)
	}
	stream.learnEchoLag(lag*20, false)
	if r = stream.acousticEchoCheck(1); echoSoundAloneVetoes(r) {
		t.Fatalf("a weak confirmation proved the delay: %+v", r)
	}
	stream.learnEchoLag(lag*20, true)
	if r = stream.acousticEchoCheck(1); !echoSoundAloneVetoes(r) {
		t.Fatalf("a proven echo path no longer vetoes: %+v", r)
	}
	stream.learnEchoLag(lag*20+400, true)
	if r = stream.acousticEchoCheck(1); echoSoundAloneVetoes(r) {
		t.Fatalf("a match at another delay vetoed: %+v", r)
	}
	t.Setenv("ECHO_VETO_SOUND_ALONE", "1")
	if !echoSoundAloneVetoes(r) {
		t.Fatal("ECHO_VETO_SOUND_ALONE=1 does not restore the veto")
	}
}
