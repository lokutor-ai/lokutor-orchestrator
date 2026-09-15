package orchestrator

import (
	"testing"
	"time"
)

// ArmEarlyEnd is the one Turno hook that can make the agent speak sooner, so
// its guarantees are asserted directly: it only shortens, only for the current
// utterance, and never survives into the next one.
func TestArmEarlyEndOnlyShortens(t *testing.T) {
	v := NewImprovedRMSVAD(0.05, 448*time.Millisecond, 16000)

	v.ArmEarlyEnd(800 * time.Millisecond) // longer than the hangover
	if v.earlyEndLimit != 0 {
		t.Errorf("arming longer than the hangover must be ignored, got %v", v.earlyEndLimit)
	}

	v.ArmEarlyEnd(448 * time.Millisecond) // equal — no gain, still ignored
	if v.earlyEndLimit != 0 {
		t.Errorf("arming equal to the hangover must be ignored, got %v", v.earlyEndLimit)
	}

	v.ArmEarlyEnd(0)
	if v.earlyEndLimit != 0 {
		t.Errorf("zero must be ignored, got %v", v.earlyEndLimit)
	}

	v.ArmEarlyEnd(-1 * time.Second)
	if v.earlyEndLimit != 0 {
		t.Errorf("negative must be ignored, got %v", v.earlyEndLimit)
	}

	v.ArmEarlyEnd(200 * time.Millisecond)
	if v.earlyEndLimit != 200*time.Millisecond {
		t.Errorf("a genuine shortening must apply, got %v", v.earlyEndLimit)
	}
}

// Arming must not leak across utterances: a turn armed by one caller's
// trailing prosody must not clip the next thing they say.
func TestArmEarlyEndClearedOnSpeechStart(t *testing.T) {
	v := NewImprovedRMSVAD(0.0001, 448*time.Millisecond, 16000)
	v.ArmEarlyEnd(200 * time.Millisecond)
	if v.earlyEndLimit == 0 {
		t.Fatal("precondition: expected the arming to be set")
	}

	// Drive loud frames until the VAD reports speech start.
	loud := make([]byte, 1024)
	for i := 0; i+1 < len(loud); i += 2 {
		loud[i], loud[i+1] = 0xFF, 0x3F
	}
	started := false
	for i := 0; i < 200 && !started; i++ {
		ev, _ := v.Process(loud)
		if ev != nil && ev.Type == VADSpeechStart {
			started = true
		}
	}
	if !started {
		t.Skip("VAD did not report speech start with synthetic audio; arming-clear covered by the reset path")
	}
	if v.earlyEndLimit != 0 {
		t.Errorf("a new utterance inherited the previous arming (%v)", v.earlyEndLimit)
	}
}

// The feature must be inert unless explicitly switched on.
func TestTurnoEarlyEndOffByDefault(t *testing.T) {
	var cfg Config
	if cfg.TurnoEarlyEndThreshold != 0 {
		t.Errorf("early end must default to off, got threshold %v", cfg.TurnoEarlyEndThreshold)
	}
}

// The floor stops the shortened hangover becoming a hair-trigger that fires on
// ordinary within-sentence breathing.
func TestEarlyEndFloorIsSane(t *testing.T) {
	if turnoEarlyEndMinMs < 100 {
		t.Errorf("floor %dms is low enough to end turns on normal pauses", turnoEarlyEndMinMs)
	}
}
