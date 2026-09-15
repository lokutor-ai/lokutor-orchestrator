package orchestrator

import (
	"strings"
	"testing"
)

// The bug this guards: an outbound agent opened by asking the person it had
// just cold-called "how can I help you?", because the orchestrator injected
// DefaultOpeningInstruction as a USER-role message, which outranks the agent's
// own system prompt for that turn. Reported by a customer testing an outbound
// sales agent that was configured to pitch.
func TestResolveOpening(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantVerbat string
		wantInstr  string
	}{
		{
			name:      "default prescribes no content",
			cfg:       Config{},
			wantInstr: OpeningTrigger,
		},
		{
			name:      "explicit instruction is honoured",
			cfg:       Config{OpeningInstruction: "Open with the Q3 promo."},
			wantInstr: "Open with the Q3 promo.",
		},
		{
			name:       "configured message is spoken verbatim, no LLM",
			cfg:        Config{OpeningMessage: "Hola, le llamo de Acme."},
			wantVerbat: "Hola, le llamo de Acme.",
		},
		{
			name: "verbatim message beats any instruction",
			cfg: Config{
				OpeningMessage:     "Hola, le llamo de Acme.",
				OpeningInstruction: "Open with the Q3 promo.",
			},
			wantVerbat: "Hola, le llamo de Acme.",
		},
		{
			name:      "whitespace-only message falls back to the instruction",
			cfg:       Config{OpeningMessage: "   \n ", OpeningInstruction: "Open with the Q3 promo."},
			wantInstr: "Open with the Q3 promo.",
		},
		{
			name:      "whitespace-only instruction falls back to the default",
			cfg:       Config{OpeningInstruction: "  "},
			wantInstr: OpeningTrigger,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotVerbat, gotInstr := resolveOpening(tc.cfg)
			if gotVerbat != tc.wantVerbat {
				t.Errorf("verbatim = %q, want %q", gotVerbat, tc.wantVerbat)
			}
			if gotInstr != tc.wantInstr {
				t.Errorf("instruction = %q, want %q", gotInstr, tc.wantInstr)
			}
			if gotVerbat != "" && gotInstr != "" {
				t.Errorf("both results non-empty: %q / %q", gotVerbat, gotInstr)
			}
		})
	}
}

// The default trigger must not put words in the agent's mouth. The whole bug
// was the orchestrator prescribing content that outranked the agent's prompt,
// so assert on the text rather than trusting that nobody reintroduces it.
func TestOpeningTriggerPrescribesNoContent(t *testing.T) {
	lower := strings.ToLower(OpeningTrigger)
	for _, banned := range []string{
		"how can i help", "how you can help", "greeting", "greet",
		"hello", "hi there", "good morning", "welcome",
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("OpeningTrigger prescribes content (%q): %q", banned, OpeningTrigger)
		}
	}
}

// With nothing configured, the agent improvises from its own prompt — there is
// no orchestrator-supplied script and no verbatim line.
func TestDefaultOpeningIsLLMDriven(t *testing.T) {
	verbatim, instr := resolveOpening(Config{})
	if verbatim != "" {
		t.Errorf("default should not speak a canned line, got %q", verbatim)
	}
	if instr != OpeningTrigger {
		t.Errorf("default instruction = %q, want OpeningTrigger", instr)
	}
}

// The horizon assist may only ever SHORTEN the confirmation wait, never remove
// it or extend it. It runs after VAD end-of-turn, so a false positive costs a
// slightly-early answer — but if it could drive the wait to zero the user would
// lose the window to reclaim a turn they were still finishing.
func TestHorizonAssistOnlyShortensWithinFloor(t *testing.T) {
	cases := []struct{ wait, floor int; factor float64; want int }{
		{600, 120, 0.25, 150}, // phone: 600 -> 150
		{200, 120, 0.25, 120}, // web: 0.25*200=50, floored to 120
		{400, 120, 0.25, 120}, // 100 -> floored
		{800, 120, 0.25, 200}, // default
	}
	for _, c := range cases {
		got := int(float64(c.wait) * c.factor)
		if got < c.floor {
			got = c.floor
		}
		if got != c.want {
			t.Errorf("wait=%d factor=%.2f floor=%d: got %d, want %d", c.wait, c.factor, c.floor, got, c.want)
		}
		if got > c.wait {
			t.Errorf("wait=%d: assist produced %d, which is LONGER than the unassisted wait", c.wait, got)
		}
		if got <= 0 {
			t.Errorf("wait=%d: assist produced %d — the user must keep a window to resume", c.wait, got)
		}
	}
}

// 0.5 would never fire: the v6 horizon head peaks around 0.48 on real speech.
func TestHorizonThresholdIsReachable(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.TurnoHorizonAssistThreshold <= 0 {
		t.Fatal("horizon assist disabled by default")
	}
	if cfg.TurnoHorizonAssistThreshold >= 0.48 {
		t.Errorf("threshold %.2f is at or above the head's observed ceiling (~0.48) — it would never fire",
			cfg.TurnoHorizonAssistThreshold)
	}
}

// The hold and the horizon assist push in opposite directions, and they must
// never both apply to the same turn — shortening a wait that exists precisely
// because prosody says the speaker is still going would reintroduce the bug.
func TestHoldAndHorizonAreMutuallyExclusive(t *testing.T) {
	cfg := DefaultConfig()
	// The hold ships disabled (see TestHoldShipsOffUntilTheHeadIsCalibrated),
	// so configure a threshold explicitly: what is under test here is the
	// mutual-exclusion logic, which must stay correct for whatever value an
	// operator sets, not the shipped default.
	cfg.TurnoHoldThreshold = 0.45
	type turn struct {
		lexicalComplete bool
		pIncomplete, pWait, pEnd200 float32
	}
	check := func(name string, tr turn, wantHold, wantAssist bool) {
		t.Helper()
		hold := cfg.TurnoHoldThreshold > 0 && (tr.pIncomplete+tr.pWait) >= cfg.TurnoHoldThreshold
		assist := cfg.TurnoHorizonAssistThreshold > 0 && !tr.lexicalComplete && !hold &&
			tr.pEnd200 >= cfg.TurnoHorizonAssistThreshold
		if hold != wantHold || assist != wantAssist {
			t.Errorf("%s: hold=%v assist=%v, want hold=%v assist=%v", name, hold, assist, wantHold, wantAssist)
		}
		if hold && assist {
			t.Errorf("%s: both hold and assist applied to one turn", name)
		}
	}
	// Text reads complete, prosody says still going -> hold, no assist.
	check("complete text, prosody says continuing", turn{true, 0.40, 0.20, 0.40}, true, false)
	// Text reads incomplete, horizon says ending -> assist, no hold.
	check("incomplete text, horizon says ending", turn{false, 0.10, 0.05, 0.40}, false, true)
	// Incomplete text AND prosody says continuing -> hold wins, full wait kept.
	check("incomplete text, prosody says continuing", turn{false, 0.40, 0.20, 0.45}, true, false)
	// Nothing conclusive -> neither.
	check("no signal", turn{true, 0.10, 0.05, 0.10}, false, false)
}

// The hold is off until the v6 head stops calling finished sentences
// unfinished. It fires on p_incomplete + p_wait, and the head reports
// p_incomplete of 0.67-0.72 on plainly complete utterances — so at the old
// default of 0.45 it fired on essentially every turn. Measured in production
// on 2026-09-15: all three turns of a live call logged "Turno hold" and paid
// the full 350ms, on transcripts like "Hello, how are you?".
//
// Turning it back on is a deliberate act that should follow shadow evidence,
// not an accident of editing DefaultConfig.
func TestHoldShipsOffUntilTheHeadIsCalibrated(t *testing.T) {
	if got := DefaultConfig().TurnoHoldThreshold; got != 0 {
		t.Errorf("TurnoHoldThreshold = %.2f, want 0 — re-enabling costs %dms on every turn and needs shadow data first",
			got, DefaultConfig().TurnoHoldMs)
	}
}

// Holding must never cost more than the mid-thought wait it sits beside: the
// text did look finished, so this is a grace window, not a full re-wait.
// Checks TurnoHoldMs, which stays configured even while the hold is disabled,
// so the duration is still sane whenever it is switched back on.
func TestHoldIsShorterThanTheMidThoughtWait(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.TurnoHoldMs <= 0 {
		t.Fatal("hold duration unset — it must stay sane for when the hold is re-enabled")
	}
	if cfg.TurnoHoldMs >= cfg.SilenceConfirmationMs && cfg.SilenceConfirmationMs > 0 {
		t.Errorf("hold %dms is not shorter than the mid-thought wait %dms",
			cfg.TurnoHoldMs, cfg.SilenceConfirmationMs)
	}
}

// The recording notice is a consent disclosure, not a nicety: if it stops
// being spoken, or stops coming first, recorded calls become unlawful in
// all-party-consent jurisdictions. These assertions exist so that failure is
// a red test rather than a legal problem discovered later.
func TestRecordingNoticeAlwaysLeadsTheOpening(t *testing.T) {
	const notice = "This call is recorded."

	t.Run("prepended to a verbatim opening", func(t *testing.T) {
		verbatim, instr := resolveOpening(Config{
			RecordingNotice: notice,
			OpeningMessage:  "Hi, this is Nova from Acme.",
		})
		if instr != "" {
			t.Fatalf("expected a verbatim opening, got instruction %q", instr)
		}
		if !strings.HasPrefix(verbatim, notice) {
			t.Errorf("notice must come first, got %q", verbatim)
		}
		if !strings.Contains(verbatim, "Hi, this is Nova from Acme.") {
			t.Errorf("the configured opening must survive, got %q", verbatim)
		}
	})

	t.Run("no notice configured leaves the opening untouched", func(t *testing.T) {
		verbatim, _ := resolveOpening(Config{OpeningMessage: "Hi, this is Nova."})
		if verbatim != "Hi, this is Nova." {
			t.Errorf("unrecorded calls must not gain a notice, got %q", verbatim)
		}
	})

	t.Run("LLM opening still yields an instruction, notice spoken separately", func(t *testing.T) {
		verbatim, instr := resolveOpening(Config{RecordingNotice: notice})
		if verbatim != "" {
			t.Errorf("with no OpeningMessage the notice is spoken by the caller of resolveOpening, not folded in; got %q", verbatim)
		}
		if instr == "" {
			t.Error("the model must still be asked to open")
		}
	})

	t.Run("whitespace-only notice is not spoken", func(t *testing.T) {
		verbatim, _ := resolveOpening(Config{RecordingNotice: "   ", OpeningMessage: "Hello."})
		if verbatim != "Hello." {
			t.Errorf("a blank notice must not prepend whitespace, got %q", verbatim)
		}
	})
}
