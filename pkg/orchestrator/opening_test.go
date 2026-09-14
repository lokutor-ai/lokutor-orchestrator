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
