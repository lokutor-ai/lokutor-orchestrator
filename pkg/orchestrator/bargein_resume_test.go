package orchestrator

import "testing"

// A rejected barge-in must never resume playback while VAD still reports
// speech. The rejection reasons (under minDur, isLikelyNoise, fewer than
// MinWordsToInterrupt words) all fire routinely mid-sentence — someone
// remembers something and starts again, and the first fragment is too short to
// clear the gate. Resuming there talks straight over a speaking human.
func TestRejectedBargeInDoesNotResumeWhileUserSpeaks(t *testing.T) {
	cases := []struct {
		name        string
		vadSpeaking bool
		ttsAlive    bool
		wantState   StreamState
	}{
		{"still speaking, tts alive", true, true, StateListening},
		{"still speaking, tts gone", true, false, StateListening},
		{"stopped, tts alive -> resume", false, true, StateSpeaking},
		{"stopped, nothing alive -> idle", false, false, StateIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := &ManagedStream{pendingBargeIn: true, vadSpeaking: tc.vadSpeaking}
			if tc.ttsAlive {
				ms.ttsCancel = func() {}
			}
			ms.resolvePendingBargeIn()
			if ms.state != tc.wantState {
				t.Errorf("state = %v, want %v", ms.state, tc.wantState)
			}
			if tc.vadSpeaking && ms.state == StateSpeaking {
				t.Error("resumed playback while the caller was still speaking")
			}
		})
	}
}

// Every state the stream can idle in must be rescuable by the silence nudge.
// StateListening was missing from that set, which is how a dropped or
// discarded utterance could strand a call in permanent silence: the caller
// spoke, nothing came back, and nothing ever checked on them again.
func TestIdleStatesAreAllRecoverable(t *testing.T) {
	recoverable := func(s StreamState) bool {
		return s == StateIdle || s == StateInterrupted || s == StateListening
	}
	// States where the stream is genuinely waiting on the caller with no work
	// of its own in flight — each must be able to trigger the nudge.
	for _, s := range []StreamState{StateIdle, StateInterrupted, StateListening} {
		if !recoverable(s) {
			t.Errorf("%v is an idle-ish state but cannot trigger the silence nudge", s)
		}
	}
	// States with work in flight must NOT be nudged — the caller is about to
	// be answered and a nudge would talk over the answer.
	for _, s := range []StreamState{StateProcessing, StateSpeaking} {
		if recoverable(s) {
			t.Errorf("%v has work in flight and must not be nudged", s)
		}
	}
}

// The stuck-VAD ceiling has to be generous enough that a real monologue never
// trips it, but finite — an infinite ceiling is the bug it exists to fix.
func TestMaxUtteranceCeilingIsSaneAndFinite(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxUtteranceSec <= 0 {
		t.Fatal("no utterance ceiling: a latched VAD would strand the call forever")
	}
	if cfg.MaxUtteranceSec < 30 {
		t.Errorf("ceiling %ds is short enough to cut off a real monologue", cfg.MaxUtteranceSec)
	}
	if cfg.MaxUtteranceSec > 300 {
		t.Errorf("ceiling %ds leaves a stuck call silent for minutes", cfg.MaxUtteranceSec)
	}
}
