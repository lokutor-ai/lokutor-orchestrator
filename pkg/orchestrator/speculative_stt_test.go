package orchestrator

import (
	"context"
	"testing"
	"time"
)

// 16kHz mono PCM16 => 32 bytes per millisecond.
const testBytesPerMs = 32

func TestSpecSTTAcceptedWhenOnlyHangoverSilenceFollows(t *testing.T) {
	var s specSTT
	snap := 32000 // 1s of audio
	if !s.begin(snap, 7) {
		t.Fatal("begin refused on a fresh attempt")
	}
	s.finish(TranscriptionResult{Text: "what are your opening hours"}, nil)

	// The hangover appended ~480ms of silence and nothing else.
	final := snap + 480*testBytesPerMs
	res, ok, _ := s.awaitUsable(context.Background(), 7, final, testBytesPerMs, defaultSpecSTTMaxTailMs)
	if !ok {
		t.Fatal("a snapshot followed only by hangover silence must be usable")
	}
	if res.Text != "what are your opening hours" {
		t.Errorf("got %q", res.Text)
	}
}

// The failure this design must not have: the caller paused, we speculated, then
// they carried on. Using that transcript would answer half a sentence.
func TestSpecSTTRejectedWhenCallerResumedSpeaking(t *testing.T) {
	var s specSTT
	snap := 32000
	if !s.begin(snap, 7) {
		t.Fatal("begin refused")
	}
	s.finish(TranscriptionResult{Text: "what are your"}, nil)

	// They spoke for another second and then the real hangover ran.
	final := snap + (1000+480)*testBytesPerMs
	if _, ok, _ := s.awaitUsable(context.Background(), 7, final, testBytesPerMs, defaultSpecSTTMaxTailMs); ok {
		t.Error("a snapshot taken before the caller resumed must be rejected")
	}
}

func TestSpecSTTRejectedForADifferentUtterance(t *testing.T) {
	var s specSTT
	snap := 32000
	s.begin(snap, 7)
	s.finish(TranscriptionResult{Text: "stale"}, nil)

	final := snap + 100*testBytesPerMs
	if _, ok, _ := s.awaitUsable(context.Background(), 8, final, testBytesPerMs, defaultSpecSTTMaxTailMs); ok {
		t.Error("a result from utterance 7 must not be used for utterance 8")
	}
}

func TestSpecSTTRejectedOnErrorOrEmptyText(t *testing.T) {
	for _, c := range []struct {
		name string
		res  TranscriptionResult
		err  error
	}{
		{"transcription error", TranscriptionResult{}, context.DeadlineExceeded},
		{"empty transcript", TranscriptionResult{Text: "   "}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var s specSTT
			snap := 32000
			s.begin(snap, 1)
			s.finish(c.res, c.err)
			final := snap + 100*testBytesPerMs
			if _, ok, _ := s.awaitUsable(context.Background(), 1, final, testBytesPerMs, defaultSpecSTTMaxTailMs); ok {
				t.Error("must fall through to the normal blocking transcription")
			}
		})
	}
}

// Only one speculative pass per pause: each one costs a Parakeet call, and the
// audio loop asks on every 32ms chunk of the hangover.
func TestSpecSTTBeginIsSingleFlight(t *testing.T) {
	var s specSTT
	if !s.begin(1000, 1) {
		t.Fatal("first begin must claim the attempt")
	}
	if s.begin(2000, 1) {
		t.Error("second begin while in flight must be refused")
	}
	s.finish(TranscriptionResult{Text: "hi"}, nil)
	if s.begin(3000, 1) {
		t.Error("begin must stay refused while an unconsumed result is held")
	}
	s.invalidate()
	if !s.begin(4000, 2) {
		t.Error("begin must be allowed again after invalidate")
	}
}

// invalidate runs on every chunk where speech resumed, so it must be safe
// against an in-flight pass and must not leave a waiter blocked forever.
func TestSpecSTTInvalidateReleasesAnInFlightWaiter(t *testing.T) {
	var s specSTT
	s.begin(1000, 1)
	s.invalidate()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.awaitUsable(ctx, 1, 1000, testBytesPerMs, defaultSpecSTTMaxTailMs)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitUsable blocked after invalidate")
	}
}

// A late result from a pass that was already invalidated must not resurrect it.
func TestSpecSTTFinishAfterInvalidateIsIgnored(t *testing.T) {
	var s specSTT
	s.begin(1000, 1)
	s.invalidate()
	s.finish(TranscriptionResult{Text: "too late"}, nil)

	if _, ok, _ := s.awaitUsable(context.Background(), 1, 1000, testBytesPerMs, defaultSpecSTTMaxTailMs); ok {
		t.Error("a result delivered after invalidate must not be used")
	}
}

func TestSpecSTTEnabledByDefault(t *testing.T) {
	t.Setenv("SPECULATIVE_STT", "")
	if !specSTTEnabled() {
		t.Error("speculative STT should be on by default")
	}
	for _, v := range []string{"false", "0", "off", "OFF"} {
		t.Setenv("SPECULATIVE_STT", v)
		if specSTTEnabled() {
			t.Errorf("SPECULATIVE_STT=%q should disable it", v)
		}
	}
}
