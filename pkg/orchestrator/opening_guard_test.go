package orchestrator

import (
	"testing"
	"time"
)

// 2026-11-24, web: the opening's first second came back through the microphone as speech, playback stopped and the
// greeting was said again. Short speech begun there neither stops nor answers it; longer speech, later speech and any reply after a caller turn behave as before.
func TestOpeningGuard(t *testing.T) {
	cases := []struct {
		name              string
		heardMs, talkedMs int
		durMs             int
		afterTurn, off    bool
		stops, drops      bool
	}{
		{"echo of the opening", 450, 800, 900, false, false, false, true},
		{"keeps talking", 450, 1100, 1200, false, false, true, false},
		{"a short sound begun just before the opening", 0, 450, 500, false, false, false, true},
		{"a caller talking since before the opening", 0, 1100, 1300, false, false, true, false},
		{"later in the opening", 1100, 450, 500, false, false, true, false},
		{"after a caller turn", 450, 450, 500, true, false, true, false},
		{"switched off", 450, 450, 500, false, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.off {
				t.Setenv("OPENING_GUARD_MS", "0")
			}
			stream := playThroughStream(t)
			stream.onVADStart(StateIdle)
			now := time.Now()
			stream.mu.Lock()
			stream.lastUtt, stream.carry = nil, nil
			if c.afterTurn {
				stream.lastUtt = &committedUtterance{}
			}
			stream.onset.heardAtStart = time.Duration(c.heardMs) * time.Millisecond
			stream.onset.at = now.Add(-time.Duration(c.talkedMs) * time.Millisecond)
			seq := stream.onset.seq
			stream.mu.Unlock()
			stream.stopPlayThroughIfDue(now)
			_, stopped := drainUntil(stream, 100*time.Millisecond)[StopPlayback]
			if stopped != c.stops {
				t.Fatalf("stopped = %v, want %v", stopped, c.stops)
			}
			if got := stream.dropOverOpening(seq, time.Duration(c.durMs)*time.Millisecond); got != c.drops {
				t.Fatalf("dropped = %v, want %v", got, c.drops)
			}
		})
	}
}
