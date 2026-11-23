package orchestrator

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func newEchoFeedStream(t *testing.T, disableEchoVeto, playThrough bool, rate int) *ManagedStream {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DisableEchoVeto = disableEchoVeto
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hi"}, &MockLLMProvider{completeResult: "ok"},
		&MockTTSProvider{synthesizeResult: []byte("a")}, NewRMSVAD(0.05, 50*time.Millisecond), cfg)
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("echo-feed"))
	t.Cleanup(func() { stream.Close() })
	stream.SetPlaybackRate(rate)
	stream.SetTransportPlaysThroughBargeIn(playThrough)
	stream.mu.Lock()
	stream.state = StateSpeaking
	stream.mu.Unlock()
	return stream
}

// sendFrames emits a reply's frames the way speakText does and returns what reached the transport.
func sendFrames(t *testing.T, stream *ManagedStream, frames [][]byte) [][]byte {
	t.Helper()
	for _, f := range frames {
		stream.emitWithGen(AudioChunk, f, 1)
	}
	var got [][]byte
	deadline := time.After(2 * time.Second)
	for len(got) < len(frames) {
		select {
		case ev := <-stream.Events():
			if ev.Type == AudioChunk {
				got = append(got, ev.Data.([]byte))
			}
		case <-deadline:
			t.Fatalf("only %d of %d frames reached the transport", len(got), len(frames))
		}
	}
	return got
}

func echoFeedState(stream *ManagedStream) (pending, farBytes int) {
	stream.echoMu.Lock()
	defer stream.echoMu.Unlock()
	return len(stream.echoFarPending), len(stream.echoFarEndBuf)
}

func replyFrames(rate int) [][]byte {
	frame := int(float64(rate)*0.06) * 2
	var frames [][]byte
	for i := 0; i < 5; i++ {
		f := make([]byte, frame)
		for j := range f {
			f[j] = byte(i*31 + j*7)
		}
		frames = append(frames, f)
	}
	return frames
}

// Nothing reads the echo reference on a phone call (echo veto off, no play-through): it is not fed, and the
// caller hears exactly the same bytes.
func TestEchoReferenceNotFedWhenNothingReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name              string
		disable, playThru bool
		wantFed           bool
	}{
		{"echo veto on (web, phone with TELNYX_ECHO_VETO=1)", false, false, true},
		{"echo veto on, play-through (web)", false, true, true},
		{"echo veto off, play-through (its stop check reads it)", true, true, true},
		{"echo veto off, no play-through (phone since 2026-11-19)", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, rate := range []int{8000, 44100} {
				stream := newEchoFeedStream(t, tc.disable, tc.playThru, rate)
				frames := replyFrames(rate)
				got := sendFrames(t, stream, frames)
				for i := range frames {
					if !bytes.Equal(got[i], frames[i]) {
						t.Fatalf("rate %d: frame %d reached the transport changed", rate, i)
					}
				}
				pending, _ := echoFeedState(stream)
				if fed := pending > 0; fed != tc.wantFed {
					t.Fatalf("rate %d: far-end echo queue holds %d chunks, want fed=%v", rate, pending, tc.wantFed)
				}

				// The microphone side: the speaker mirror advances on every caller frame only when it is read.
				mic := make([]byte, 320*2)
				stream.handleAudio(mic)
				time.Sleep(30 * time.Millisecond)
				stream.handleAudio(mic)
				_, far := echoFeedState(stream)
				if fed := far > 0; fed != tc.wantFed {
					t.Fatalf("rate %d: far-end mirror holds %d bytes after microphone frames, want fed=%v", rate, far, tc.wantFed)
				}
			}
		})
	}
}
