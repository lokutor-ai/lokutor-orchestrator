package orchestrator

import (
	"context"
	"testing"
	"time"
)

type timeT = time.Time

func timeNowForTest() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) }

// noopSpeakLogger keeps the guard's log line out of test output.
type noopSpeakLogger struct{}

func (noopSpeakLogger) Debug(string, ...interface{}) {}
func (noopSpeakLogger) Info(string, ...interface{})  {}
func (noopSpeakLogger) Warn(string, ...interface{})  {}
func (noopSpeakLogger) Error(string, ...interface{}) {}

// onVADEnd removes the pre-generation wait on the explicit promise that a late
// recheck catches a caller who resumed talking "right before speakText plays
// anything". That recheck did not exist, so a caller who started speaking again
// during generation was spoken over: no bot audio existed yet, so nothing
// registered as a barge-in and playback simply began on top of them.
func TestSpeakTextDiscardsResponseWhenCallerResumed(t *testing.T) {
	ms := &ManagedStream{
		orch:   &Orchestrator{config: Config{}},
		logger: noopSpeakLogger{},
		state:  StateProcessing,
	}
	ms.vadSpeaking = true // the caller started talking again during generation

	ms.speakText(context.Background(), "a response they have stopped waiting for", 0)

	if ms.state == StateSpeaking {
		t.Error("began playback while the caller was speaking")
	}
	if ms.state != StateListening {
		t.Errorf("state = %v, want StateListening so the new utterance is handled", ms.state)
	}
	if !ms.botSpeakStart.IsZero() {
		t.Error("playback timing was started despite discarding the response")
	}
}

// The guard must not make the agent mute: with the caller silent, the normal
// path still has to reach playback.
func TestSpeakTextProceedsWhenCallerSilent(t *testing.T) {
	ms := &ManagedStream{
		orch:   &Orchestrator{config: Config{}},
		logger: noopSpeakLogger{},
		state:  StateProcessing,
	}
	ms.vadSpeaking = false

	// A cancelled context stops it before real TTS work, but only *after* the
	// vadSpeaking guard — so reaching the context check proves the guard let
	// it through rather than swallowing the turn.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ms.speakText(ctx, "hello", 0)

	if ms.state == StateListening {
		t.Error("a silent caller must not cause the response to be discarded as a resume")
	}
}

// stageMs must distinguish "not measured" from "instant". A zero would read as
// a stage that took no time, which is exactly the wrong conclusion to draw
// when optimising.
func TestStageMsReportsUnmeasuredAsNegative(t *testing.T) {
	base := timeNowForTest()
	cases := []struct {
		name     string
		from, to timeT
		want     int64
	}{
		{"both unset", timeT{}, timeT{}, -1},
		{"from unset", timeT{}, base, -1},
		{"to unset", base, timeT{}, -1},
		{"reversed", base.Add(100 * time.Millisecond), base, -1},
		{"measured", base, base.Add(250 * time.Millisecond), 250},
	}
	for _, tc := range cases {
		if got := stageMs(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: stageMs = %d, want %d", tc.name, got, tc.want)
		}
	}
}
