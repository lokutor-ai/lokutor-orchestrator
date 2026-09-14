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
