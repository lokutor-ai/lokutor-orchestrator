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
			name:      "default is the inbound greeting",
			cfg:       Config{},
			wantInstr: DefaultOpeningInstruction,
		},
		{
			name:      "outbound defers to the agent's own prompt",
			cfg:       Config{OpeningInstruction: OutboundOpeningInstruction},
			wantInstr: OutboundOpeningInstruction,
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
				OpeningInstruction: OutboundOpeningInstruction,
			},
			wantVerbat: "Hola, le llamo de Acme.",
		},
		{
			name:      "whitespace-only message falls back to the instruction",
			cfg:       Config{OpeningMessage: "   \n ", OpeningInstruction: OutboundOpeningInstruction},
			wantInstr: OutboundOpeningInstruction,
		},
		{
			name:      "whitespace-only instruction falls back to the default",
			cfg:       Config{OpeningInstruction: "  "},
			wantInstr: DefaultOpeningInstruction,
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

// The outbound instruction must not itself reintroduce the behavior it exists
// to prevent. SkipBotGreeting was set for outbound for exactly this purpose and
// silently did nothing, so assert on content rather than trusting a flag.
func TestOutboundOpeningDoesNotAskHowToHelp(t *testing.T) {
	lower := strings.ToLower(OutboundOpeningInstruction)
	for _, bad := range []string{"ask how you can help", "how can i help"} {
		if strings.Contains(lower, bad) && !strings.Contains(lower, "do not ask") {
			t.Errorf("outbound opening instruction still tells the model to offer help: %q",
				OutboundOpeningInstruction)
		}
	}
	if !strings.Contains(lower, "do not ask") {
		t.Error("outbound opening instruction should explicitly rule out the greeting-and-yield behavior")
	}
}

// SkipBotGreeting is dead. If someone revives it as a real control they must
// also wire it into the opening path; this documents that it is currently inert
// so a future reader doesn't set it and assume it took effect.
func TestSkipBotGreetingIsInert(t *testing.T) {
	_, withFlag := resolveOpening(Config{SkipBotGreeting: true})
	_, without := resolveOpening(Config{})
	if withFlag != without {
		t.Fatalf("SkipBotGreeting now changes behavior (%q vs %q) — update its doc comment "+
			"and the outbound path in voice_agent_telnyx.go", withFlag, without)
	}
}
