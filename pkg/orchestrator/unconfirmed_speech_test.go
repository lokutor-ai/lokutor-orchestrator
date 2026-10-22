package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// scriptVAD reports speech while speaking is set, and a low probability otherwise. It raises no events, so no
// utterance is processed: the test reads only what handleAudio logs.
type scriptVAD struct{ speaking bool }

func (v *scriptVAD) Process(chunk []byte) (*VADEvent, error) { return nil, nil }
func (v *scriptVAD) IsSpeaking() bool                        { return v.speaking }
func (v *scriptVAD) Reset()                                  {}
func (v *scriptVAD) Clone() VADProvider                      { return v }
func (v *scriptVAD) Name() string                            { return "script" }
func (v *scriptVAD) LastProbability() float64 {
	if v.speaking {
		return 0.9
	}
	return 0.02
}
func (v *scriptVAD) LastEffectiveThreshold() float64 { return 0.06 }

// The rest of a sentence the VAD scored under its threshold left no trace (2026-10-21: "How does Lok-" answered, then
// "-utor compare to ElevenLabs?" at full level, lastProbability 0.02). Speech-level audio the VAD never confirms is
// logged once per gap; quiet audio, a burst the VAD goes on to confirm, and audio before any confirmed speech are not.
func TestUnconfirmedSpeechLeavesEvidence(t *testing.T) {
	const line = "INFO Caller audio at speech level that the VAD did not confirm as speech"
	type step struct {
		db       float64 // 0 = digital silence
		ms       int
		speaking bool
	}
	for _, c := range []struct {
		name  string
		steps []step
		want  int
	}{
		{"the rest of the sentence after the VAD let go", []step{{-20, 600, true}, {-24, 1200, false}, {0, 800, false}}, 1},
		{"two unconfirmed phrases in one gap: one line", []step{{-20, 600, true}, {-24, 600, false}, {0, 700, false}, {-22, 600, false}, {0, 700, false}}, 1},
		{"one per gap", []step{{-20, 600, true}, {-24, 600, false}, {0, 700, false}, {-20, 400, true}, {-24, 600, false}, {0, 700, false}}, 2},
		{"a long unconfirmed stretch is reported while it goes on", []step{{-20, 600, true}, {-24, 2400, false}}, 1},
		{"the VAD confirms the burst: its lead-in carries it", []step{{-20, 600, true}, {0, 600, false}, {-24, 280, false}, {-22, 600, true}, {0, 700, false}}, 0},
		{"quiet under the line: the room, a soft echo", []step{{-20, 600, true}, {-45, 1500, false}, {0, 700, false}}, 0},
		{"a short sound", []step{{-20, 600, true}, {-24, 200, false}, {0, 700, false}}, 0},
		{"nothing to compare with before the caller's first speech", []step{{-24, 1500, false}, {0, 700, false}}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &recordingLogger{}
			cfg := DefaultConfig()
			cfg.FirstSpeaker = FirstSpeakerUser
			cfg.SampleRate = 16000
			cfg.SilenceTimeout = 0
			vad := &scriptVAD{}
			orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hola"}, &MockLLMProvider{}, &MockTTSProvider{}, vad, cfg, log)
			ms := orch.NewManagedStream(context.Background(), NewConversationSession("unconfirmed"))
			defer ms.Close()
			for _, s := range c.steps {
				vad.speaking = s.speaking
				for i := 0; i < s.ms/20; i++ {
					chunk := make([]byte, 640)
					if s.db != 0 {
						chunk = toneAt(s.db, 20)
					}
					ms.handleAudio(chunk)
				}
			}
			if got := log.count(line); got != c.want {
				var seen []string
				for _, l := range log.lines {
					if strings.Contains(l, "did not confirm") {
						seen = append(seen, l)
					}
				}
				t.Fatalf("%d evidence lines, want %d: %v", got, c.want, seen)
			}
		})
	}
}

func TestUnconfirmedSpeechReport(t *testing.T) {
	var u unconfirmedSpeech
	now := time.Now()
	feed := func(db float64, ms int, speaking bool, prob float64) (unconfirmedReport, bool) {
		var r unconfirmedReport
		var ok bool
		for i := 0; i < ms/20; i++ {
			chunk := make([]byte, 640)
			if db != 0 {
				chunk = toneAt(db, 20)
			}
			now = now.Add(20 * time.Millisecond)
			if rr, o := u.note(chunk, speaking, prob, now); o {
				r, ok = rr, true
			}
		}
		return r, ok
	}
	feed(-20, 400, true, 0.9)
	feed(-26, 700, false, 0.08)
	r, ok := feed(0, 600, false, 0.001)
	if !ok {
		t.Fatal("no report")
	}
	if r.loudMs != 700 || r.maxProb != 0.08 || r.belowDb < 5.5 || r.belowDb > 6.5 || r.sinceSpeechMs < 1100 {
		t.Fatalf("report = %+v, want 700 ms, max probability 0.08, ~6 dB under the speech peak, ~1.2 s after it", r)
	}
}
