package orchestrator

import (
	"testing"
	"time"
)

// The pre-render is audio for a reply nobody has committed to yet. Everything that decides whether
// it may be played lives in take(), so these are the guards that stop a caller hearing the right
// sentence in the wrong voice, or yesterday's answer.

func chunks(n int) [][]byte { return [][]byte{make([]byte, n)} }

func newPre(text string, v Voice, l Language) *prerendered {
	p := &prerendered{}
	p.text, p.chunks, p.voice, p.lang, p.at = text, chunks(8), v, l, time.Now()
	return p
}

func TestPrerenderIsUsedForTheExactSegment(t *testing.T) {
	p := newPre("Abre a las nueve.", VoiceF1, LanguageEs)
	if got := p.take("Abre a las nueve.", VoiceF1, LanguageEs); got == nil {
		t.Fatal("rendered audio was not used for the segment it was rendered for")
	}
}

func TestPrerenderIsSingleUse(t *testing.T) {
	p := newPre("Hola.", VoiceF1, LanguageEs)
	if p.take("Hola.", VoiceF1, LanguageEs) == nil {
		t.Fatal("first take failed")
	}
	if p.take("Hola.", VoiceF1, LanguageEs) != nil {
		t.Error("same audio served twice — the second turn would hear the first turn's sentence")
	}
}

func TestPrerenderIsRefusedForDifferentText(t *testing.T) {
	p := newPre("Abre a las nueve.", VoiceF1, LanguageEs)
	if p.take("Cierra a las ocho.", VoiceF1, LanguageEs) != nil {
		t.Error("played audio that says something else — the fast wrong answer")
	}
	// ...and a refusal must still clear, or the stale audio lingers for the next turn.
	if p.chunks != nil {
		t.Error("refused audio was left in place")
	}
}

func TestPrerenderIsRefusedAfterAVoiceOrLanguageChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		voice Voice
		lang  Language
	}{
		{"voice changed", VoiceM3, LanguageEs},
		{"language changed", VoiceF1, LanguageCa},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPre("Hola.", VoiceF1, LanguageEs)
			if p.take("Hola.", tc.voice, tc.lang) != nil {
				t.Error("audio is conditioning, not just text: a caller who switched must not " +
					"hear one sentence in the old voice")
			}
		})
	}
}

func TestPrerenderExpires(t *testing.T) {
	p := newPre("Hola.", VoiceF1, LanguageEs)
	p.at = time.Now().Add(-2 * prerenderMaxAge)
	if p.take("Hola.", VoiceF1, LanguageEs) != nil {
		t.Error("played stale audio: the conversation has moved on, so this is the wrong answer fast")
	}
}

func TestPrerenderIgnoresSurroundingWhitespace(t *testing.T) {
	p := newPre("Abre a las nueve.", VoiceF1, LanguageEs)
	if p.take("  Abre a las nueve.  ", VoiceF1, LanguageEs) == nil {
		t.Error("a trimmed copy of the same sentence should still match")
	}
}

func TestDiscardStopsAnInFlightRender(t *testing.T) {
	p := &prerendered{}
	cancelled := false
	p.text, p.chunks = "Hola.", chunks(4)
	p.cancel = func() { cancelled = true }
	p.discard()
	if !cancelled {
		t.Error("in-flight render not cancelled — it would hold a synthesiser slot the " +
			"confirmed path needs")
	}
	if p.chunks != nil || p.text != "" {
		t.Error("discard left audio behind")
	}
}

func TestEmptyPrerenderIsSafe(t *testing.T) {
	p := &prerendered{}
	if p.take("anything", VoiceF1, LanguageEs) != nil {
		t.Error("returned audio from an empty prerender")
	}
	p.discard() // must not panic with no cancel registered
}

// Pre-rendering must stay off unless someone has decided the node can spare a synthesiser slot.
// Enabled on a one-stream node it held the only slot for ~1s per turn, the confirmed path got
// "503 busy", and turns went from 353ms to 14.4 SECONDS. Default-on is the dangerous direction.
func TestPrerenderIsOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("SPECULATIVE_PRERENDER", "")
	if speculativePrerenderEnabled() {
		t.Error("default is ON; a one-stream node would starve its own confirmed path")
	}
	if DefaultConfig().SpeculativePrerender {
		t.Error("DefaultConfig enables pre-render")
	}
	for _, on := range []string{"1", "true", "TRUE"} {
		t.Setenv("SPECULATIVE_PRERENDER", on)
		if !speculativePrerenderEnabled() {
			t.Errorf("%q should enable it", on)
		}
	}
	for _, off := range []string{"0", "false", "no", "yes", "2"} {
		t.Setenv("SPECULATIVE_PRERENDER", off)
		if speculativePrerenderEnabled() {
			t.Errorf("%q should not enable it — only an explicit 1/true counts", off)
		}
	}
}
