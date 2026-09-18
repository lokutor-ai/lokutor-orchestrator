package orchestrator

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
)

// Rendering the first sentence of a reply before the turn is confirmed.
//
// By the time a turn ends, speculation has usually already produced the reply TEXT — that is why
// stt_ms and llm_ms read 0 on a hit. What it had not done is turn that text into sound, so every
// confirmed turn still paid the synthesiser. Measured across five live turns, that was the entire
// remaining budget besides the VAD hangover:
//
//	e2e 353ms = hangover 245 + tts_first 107
//	e2e 472ms = hangover 247 + tts_first 223
//
// So the first block of audio is rendered during the hangover too. On a hit it is already in hand
// and playback starts immediately; the floor becomes the hangover alone, around 245ms.
//
// Nothing here commits to anything. The hangover still decides when the turn ends, the transcript
// still has to match, and a miss throws the audio away and synthesises normally — the same contract
// speculation already had. The only new cost is wasted synthesis on a miss, which is why it renders
// ONE segment rather than the whole reply.

// prerenderMaxAge bounds how long a rendered opening stays usable. A speculation that fired several
// seconds ago describes a conversation that has moved on, and stale audio is worse than a fast
// answer: it is the wrong answer, fast.
const prerenderMaxAge = 10 * time.Second

type prerendered struct {
	mu     sync.Mutex
	text   string   // the exact segment this audio says
	voice  Voice    // conditioning it was rendered with
	lang   Language //
	chunks [][]byte
	at     time.Time
	cancel context.CancelFunc
}

// take returns the audio for `text` if it was rendered for this exact segment, voice and language,
// and clears it either way — a rendered opening is good for exactly one turn.
func (p *prerendered) take(text string, voice Voice, lang Language) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.chunks
	at, hadText, hadVoice, hadLang := p.at, p.text, p.voice, p.lang
	p.text, p.chunks, p.cancel = "", nil, nil
	if got == nil || hadText == "" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(hadText), strings.TrimSpace(text)) {
		return nil
	}
	// Voice and language are part of the audio, not of the request: a caller who switched voice
	// mid-call must not hear one sentence in the old one.
	if hadVoice != voice || hadLang != lang {
		return nil
	}
	if time.Since(at) > prerenderMaxAge {
		return nil
	}
	return got
}

// discard drops any pending render and stops one still in flight.
func (p *prerendered) discard() {
	p.mu.Lock()
	cancel := p.cancel
	p.text, p.chunks, p.cancel = "", nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// prerenderFirstSegment synthesises the opening segment of a speculative reply into memory.
//
// Runs on the speculation goroutine, inside the VAD hangover, against a reply nobody has committed
// to. It deliberately renders only the FIRST segment: that is the one that sets time-to-first-audio,
// and on a miss it is the only work thrown away.
func (ms *ManagedStream) prerenderFirstSegment(response string) {
	if ms.orch == nil || !ms.orch.config.SpeculativePrerender {
		return
	}
	segs := splitSentences(response)
	if len(segs) == 0 {
		return
	}
	first := strings.TrimSpace(segs[0])
	if first == "" {
		return
	}

	voice := ms.session.GetCurrentVoice()
	lang := ms.session.GetCurrentLanguage()

	// Its own context, cancelled by discard(): a render still running when the turn resolves must
	// not keep a synthesiser slot busy while the confirmed path is waiting for one.
	ctx, cancel := context.WithTimeout(ms.ctx, prerenderMaxAge)
	ms.prerender.mu.Lock()
	if old := ms.prerender.cancel; old != nil {
		old() // a newer speculation supersedes the last
	}
	ms.prerender.text, ms.prerender.chunks, ms.prerender.cancel = "", nil, cancel
	ms.prerender.voice, ms.prerender.lang, ms.prerender.at = voice, lang, time.Now()
	ms.prerender.mu.Unlock()

	var chunks [][]byte
	start := time.Now()
	err := ms.orch.SynthesizeStream(ctx, first, voice, lang, func(chunk []byte) error {
		c := make([]byte, len(chunk))
		copy(c, chunk)
		chunks = append(chunks, c)
		return nil
	})
	if err != nil || ctx.Err() != nil || len(chunks) == 0 {
		cancel()
		ms.prerender.mu.Lock()
		if ms.prerender.cancel != nil {
			ms.prerender.text, ms.prerender.chunks = "", nil
		}
		ms.prerender.mu.Unlock()
		return
	}

	ms.prerender.mu.Lock()
	// Only publish if this render is still the current one — a newer speculation may have
	// superseded it while this was synthesising.
	if ms.prerender.cancel != nil {
		ms.prerender.text, ms.prerender.chunks = first, chunks
	}
	ms.prerender.mu.Unlock()
	ms.logger.Info("Pre-rendered opening segment during hangover",
		"chars", len(first), "chunks", len(chunks), "render_ms", time.Since(start).Milliseconds())
}

// speculativePrerenderEnabled reports whether pre-rendering is switched on.
//
// Default off. It trades a synthesiser slot for latency, and that trade is only available on a node
// with a slot to spare — see the Config field's comment for what happened on a one-stream node.
func speculativePrerenderEnabled() bool {
	v := strings.TrimSpace(os.Getenv("SPECULATIVE_PRERENDER"))
	return v == "1" || strings.EqualFold(v, "true")
}
