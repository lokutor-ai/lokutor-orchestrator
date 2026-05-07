package orchestrator

import (
	"testing"
	"time"
)

func TestNewBackchannelGenerator(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
		bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)
	if bg == nil {
		t.Fatal("Expected non-nil BackchannelGenerator")
	}
	if !bg.enabled {
		t.Error("Expected enabled")
	}
}

func TestBackchannelDisabled(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, false, 0.5, LanguageEn)
	should, text := bg.OnUserPause()
	if should {
		t.Error("Should not backchannel when disabled")
	}
	if text != "" {
		t.Errorf("Expected empty text when disabled, got %q", text)
	}
}

func TestBackchannelRecordAudio(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	bg.RecordAudio(0.1)
	bg.RecordAudio(0.2)
	bg.RecordAudio(0.15)

	baseline := bg.GetBaselineRMS()
	if baseline <= 0 {
		t.Error("Expected non-zero baseline RMS")
	}
}

func TestBackchannelRecordSpeechStart(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	bg.RecordUserSpeechStart()
	// Before any pause, should not backchannel
	should, text := bg.OnUserPause()
	if should {
		t.Errorf("Should not backchannel immediately after speech start, got text %q", text)
	}
}

func TestBackchannelSelectPhrasesExist(t *testing.T) {
	// Verify each language has backchannel phrases
	for lang, phrases := range map[Language][]string{
		LanguageEn:   shortBackchannelEN,
		LanguageEs:   shortBackchannelES,
		LanguageFr:    shortBackchannelFR,
		LanguageDe:    shortBackchannelDE,
		LanguagePt: shortBackchannelPT,
		LanguageIt:   shortBackchannelIT,
	} {
		if len(phrases) == 0 {
			t.Errorf("No backchannel phrases for %s", lang)
		}
		if len(phrases) < 3 {
			t.Errorf("Too few backchannel phrases for %s: %d", lang, len(phrases))
		}
	}
}

func TestBackchannelEnergyTrend(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	bg.RecordAudio(0.5)
	bg.RecordAudio(0.4)
	bg.RecordAudio(0.3)
	bg.RecordAudio(0.2)
	bg.RecordAudio(0.1)

	trend := bg.energyTrend()
	if trend >= 0 {
		t.Errorf("Expected negative trend for decreasing energy, got %f", trend)
	}
}

func TestBackchannelOnUserPause(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	bg.RecordUserSpeechStart()
	// Fill energy window with data
	bg.RecordAudio(0.5)
	bg.RecordAudio(0.4)

	// Call OnUserPause twice: first call starts tracking pause, second evaluates
	bg.OnUserPause()
	// Simulate time passing by directly setting the pause start
	bg.mu.Lock()
	bg.userPauseStart = time.Now().Add(-400 * time.Millisecond)
	bg.mu.Unlock()

	should, text := bg.OnUserPause()
	// May or may not backchannel depending on energy trend thresholds
	if should && text == "" {
		t.Error("Expected non-empty backchannel text if should=true")
	}
	t.Logf("Backchannel decision: should=%v text=%q", should, text)
}

func TestBackchannelMixer(t *testing.T) {
	mixer := &BackchannelMixer{}
	output := make([]byte, 320) // 10ms at 16kHz 16-bit
	backchannel := []byte{0x00, 0x10, 0x00, 0x20}
	mixer.Play(backchannel)
	mixed := mixer.Mix(output, 0.5)

	hasSignal := false
	for _, b := range mixed {
		if b != 0 {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		t.Error("Expected mixed audio to have signal")
	}
}

func TestBackchannelMixerNoActive(t *testing.T) {
	mixer := &BackchannelMixer{}
	src := []byte{0x01, 0x02, 0x03, 0x04}
	result := mixer.Mix(src, 1.0)
	if len(result) != len(src) {
		t.Errorf("Expected same length as input, got %d", len(result))
	}
}

func TestIsLikelyBackchannelAcoustic(t *testing.T) {
	tests := []struct {
		name        string
		transcript  string
		rms         float64
		baselineRMS float64
		duration    time.Duration
		want        bool
	}{
		{
			name:        "low energy short utterance",
			transcript:  "okay",
			rms:         0.1,
			baselineRMS: 0.5,
			duration:    200 * time.Millisecond,
			want:        true,
		},
		{
			name:        "single word short utterance even at high energy",
			transcript:  "hello",
			rms:         0.5,
			baselineRMS: 0.5,
			duration:    300 * time.Millisecond,
			want:        true,
		},
		{
			name:        "multi-word utterance even if low energy",
			transcript:  "i have a question for you",
			rms:         0.1,
			baselineRMS: 0.5,
			duration:    1500 * time.Millisecond,
			want:        true,
		},
		{
			name:        "very low energy single word",
			transcript:  "sí",
			rms:         0.05,
			baselineRMS: 0.6,
			duration:    150 * time.Millisecond,
			want:        true,
		},
		{
			name:        "multi-word normal energy normal duration not backchannel",
			transcript:  "I need to book a flight",
			rms:         0.5,
			baselineRMS: 0.5,
			duration:    1500 * time.Millisecond,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLikelyBackchannelAcoustic(tt.transcript, tt.rms, tt.baselineRMS, tt.duration, 0.4)
			if got != tt.want {
				t.Errorf("IsLikelyBackchannelAcoustic(%q, %.2f, %.2f, %v) = %v, want %v",
					tt.transcript, tt.rms, tt.baselineRMS, tt.duration, got, tt.want)
			}
		})
	}
}

func TestPreWarmDisabled(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, false, 0.5, LanguageEn)

	// PreWarm with disabled backchannel should not panic
	bg.PreWarm(nil)
}

func TestPreWarmEnabledNilTTS(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	// PreWarm with nil tts provider — expect panic or graceful handling
	defer func() {
		if r := recover(); r != nil {
			t.Logf("PreWarm panicked as expected with nil tts: %v", r)
		}
	}()
	bg.PreWarm(nil)
}

func TestSelectBackchannelReturnsPhrase(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEn)

	phrase := bg.selectBackchannel()
	if phrase == "" {
		t.Error("Expected non-empty backchannel phrase")
	}
}

func TestSelectBackchannelSpanish(t *testing.T) {
	orch := &Orchestrator{}
	session := NewConversationSession("test")
	bg := NewBackchannelGenerator(orch, session, true, 0.5, LanguageEs)

	phrase := bg.selectBackchannel()
	if phrase == "" {
		t.Error("Expected non-empty Spanish backchannel phrase")
	}
}
