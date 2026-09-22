package orchestrator

import (
	"context"
	"math"
	"os"
	"strconv"
	"time"
)

// speculation_trigger.go wires SpeculativeExecutor (speculator.go) into the
// audio hot path: a fast (~100ms) in-utterance pause trigger that starts
// speculative STT+LLM generation well before the real VAD's hangover
// (~448ms, see pkg/vad/silero.go) would ever confirm end-of-turn, plus the
// shortcut in processUtterance that uses the result if the guess turns out
// to have been right. The hangover is still the only thing that commits to
// anything — this only ever gets a head start on work that would have
// happened anyway.

// pauseSpecDelay is how long a chunk-level quiet stretch must last,
// independent of and much shorter than the real VAD's hangover, before
// this stream's own pause tracker treats it as "maybe the user is done" —
// worth a speculative guess even though nothing will actually be
// confirmed or played for a few hundred more ms.
const pauseSpecDelay = 100 * time.Millisecond

// minSpecAudioMs is the least audio worth bothering to speculate on — a
// pause after only a syllable or two isn't worth an LLM call.
const minSpecAudioMs = 300

// quickChunkRMS is a fast, throwaway RMS read on raw int16 PCM, used only
// to guess whether a chunk had real energy — not the real VAD (which
// smooths through brief gaps by design via its hangover). A wrong read
// here only ever costs wasted speculative compute; it never affects what
// gets played, cut, or confirmed.
func quickChunkRMS(chunk []byte) float64 {
	n := len(chunk) / 2
	if n == 0 {
		return 0
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		s := int16(chunk[i*2]) | int16(chunk[i*2+1])<<8
		v := float64(s) / 32768.0
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(n))
}

// updatePauseSpeculationTrigger is called on every incoming chunk
// (regardless of what the hangover-smoothed VAD currently reports) to
// track raw energy and fire the fast speculative trigger on a short
// in-utterance pause.
func (ms *ManagedStream) updatePauseSpeculationTrigger(chunk []byte) {
	if ms.speculator == nil || ms.orch == nil || !ms.orch.config.SpeculativeLLM {
		return
	}

	threshold := ms.orch.config.BargeInVADThreshold
	if threshold <= 0 {
		threshold = 0.01
	}

	if quickChunkRMS(chunk) > threshold {
		ms.lastRawEnergyAt = time.Now()
		ms.specTriggeredForRun = false
		return
	}

	// Quiet chunk. Only a candidate trigger if we're actually mid-utterance.
	if ms.userSpeakingSince.IsZero() || ms.state != StateListening {
		return
	}
	if ms.specTriggeredForRun || ms.lastRawEnergyAt.IsZero() {
		return
	}
	if time.Since(ms.lastRawEnergyAt) < pauseSpecDelay {
		return
	}
	minBytes := minSpecAudioMs * ms.inputSampleRate * 2 / 1000
	if len(ms.speechAudioBuf) < minBytes {
		return
	}
	if !ms.speculator.ShouldSpeculateOnPause(ms.lastSpecAt) {
		return
	}

	ms.specTriggeredForRun = true
	ms.startSpeculation()
}

// startSpeculation launches a speculative STT+LLM run against a snapshot
// of the audio and conversation history accumulated so far. Never mutates
// ms.session — only the real, confirmed path (processUtterance) does that.
func (ms *ManagedStream) startSpeculation() {
	ms.lastSpecAt = time.Now()
	audioCopy := make([]byte, len(ms.speechAudioBuf))
	copy(audioCopy, ms.speechAudioBuf)
	history := ms.session.GetContextCopy()
	tools := ms.session.GetTools()
	ms.speculator.Start(ms.ctx, ms.orch, audioCopy, ms.session.GetCurrentLanguage(), history, tools)
}

// trySpeculativeResponse checks whether a speculative run already produced
// (or is about to produce) a response matching the just-confirmed
// transcript, and if so, speaks it directly — skipping the LLM call
// ms.runLLMAndTTS would otherwise make. Returns false on any miss
// (mismatch, failure, or timeout waiting for an in-flight run), having
// done nothing observable; the caller's only job on false is to fall
// through to the normal ms.runLLMAndTTS exactly as if this were never
// attempted. Either way, the speculator is left Idle and ready for the
// next utterance.
func (ms *ManagedStream) trySpeculativeResponse(ctx context.Context, transcript string) bool {
	// See ckEnterSpecMs's field comment: this is the one remaining unmeasured stretch in a chain of
	// checkpoints added to chase a 13-17 second ck_pre_llm_ms with everything else reading zero.
	// Ordinary function-call overhead reads near-zero; if this doesn't, the stall is the goroutine
	// not being scheduled, not anything this function's own body is doing.
	ms.mu.Lock()
	if !ms.sttEndTime.IsZero() {
		ms.ckEnterSpecMs = time.Since(ms.sttEndTime).Milliseconds()
	}
	ms.mu.Unlock()

	if ms.speculator == nil || ms.orch == nil || !ms.orch.config.SpeculativeLLM {
		return false
	}

	// Bounded wait for an in-flight run. The bound has to be smaller than the
	// thing it is an optimisation of, and at 4 seconds it was ten times larger.
	//
	// Measured on a live turn: e2e 1988ms, of which gate_spec_await_ms was
	// 1437 — the single largest term by far, and none of it work. The
	// speculation had not finished, so the turn sat waiting for a shortcut
	// while the ordinary path would have answered in about 400ms. A good turn
	// on the same pod: 610ms end to end.
	//
	// So the cap is now roughly what the real path costs. Past that point
	// waiting cannot win: even if the speculative answer lands the moment
	// after, we have already spent more than running it ourselves would have.
	// Missing is cheap — the caller falls through to runLLMAndTTS and pays the
	// normal price — so the only expensive outcome is waiting too long for a
	// hit, which is exactly what this prevents.
	awaitCtx, cancel := context.WithTimeout(ctx, speculativeAwaitBudget())
	awaitStart := time.Now()
	response, ok := ms.speculator.Await(awaitCtx, transcript)
	cancel()
	ms.mu.Lock()
	ms.specAwaitMs = time.Since(awaitStart).Milliseconds()
	ms.mu.Unlock()
	// Always clean up: Await leaves the executor in SpecReady/SpecRunning,
	// and ShouldSpeculate/ShouldSpeculateOnPause both require SpecIdle to
	// start a new run — without this, one used-or-missed speculation would
	// permanently block every later attempt for the rest of the call.
	tailStart := time.Now()
	ms.speculator.Cancel()

	if !ok {
		// Miss. Anything rendered for the guessed reply is wrong for this turn, and a render still
		// in flight would hold a synthesiser slot the confirmed path is about to need — on a node
		// with one stream slot that is the difference between answering and queueing.
		ms.prerender.discard()
		// specAwaitMs proved bounded (~350ms) in production while ckPreLLMMs (sttEnd -> entering
		// runLLMAndTTS, which a miss reaches right after this) still read 11-25 SECONDS on live
		// turns — the same "large span, every named checkpoint at zero" shape a prior 25s Catalan
		// incident left in ckPreLLMMs itself. Cancel() and prerender.discard() were the only
		// unmeasured work between them, so this either names the stall or clears both for good.
		ms.mu.Lock()
		ms.specMissTailMs = time.Since(tailStart).Milliseconds()
		ms.mu.Unlock()
		return false
	}

	rCtx, rCancel := context.WithCancel(ctx)
	ms.mu.Lock()
	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
	}
	ms.pipelineCancel = rCancel
	ms.pipelineCtx = rCtx
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()
	defer rCancel()

	// BotThinking is emitted from inside speakText (via speakResponse below), not here — see the
	// comment on that emission for why: the client SDK stops currently-playing audio the instant
	// it sees a higher generation number, so telling it before speakText's own "caller started
	// talking again" check has run risks cutting off real audio for a generation that gets
	// discarded moments later.
	ms.llmStartTime = time.Now()
	ms.llmEndTime = time.Now() // already generated ahead of time — no LLM wait on this turn
	// Same per-turn reset as runLLMAndTTS — see the comment there. Without
	// it, this turn's ttfa_ms/tts_first_ms would be measured against
	// whatever sentence last set ttsFirstChunkTime on a PRIOR turn.
	ms.ttsFirstChunkTime = time.Time{}
	// Same pairing as runLLMAndTTS: these are two ends of one measurement.
	ms.ttsStartTime = time.Time{}
	// Same per-turn reset as runLLMAndTTS — see turnAudioBytes's field comment.
	ms.turnAudioBytes = 0

	ms.mu.Lock()
	ms.lastResponseText = response
	ms.spokenTextPrefix = ""
	ms.spokenTextLocked = false
	ms.mu.Unlock()
	ms.session.AddMessage("assistant", response)
	ms.emitWithGen(BotResponse, response, gen)
	ms.cacheResponse(transcript, response, nil)

	ms.logger.Info("Speculative LLM response used", "transcript", transcript)
	ms.speakResponse(rCtx, response, gen)
	return true
}

// speculativeAwaitBudget is how long a confirmed turn will wait for an in-flight speculative run
// before giving up and generating the reply itself.
//
// It is a latency cap, not a correctness knob: waiting longer only ever produces the same answer
// later. The right value is "about what the normal path costs", because beyond that a hit is no
// longer a saving. Production turns land near 400ms end-to-end when speculation hits (hangover
// ~230ms + TTS first chunk ~280ms, with STT and LLM already done), so 350ms leaves room for a run
// that is genuinely about to finish while cutting off one that is not.
//
// Raising this is almost always the wrong instinct. A higher cap does not make hits more likely; it
// makes misses more expensive, and a miss already costs the full normal pipeline on top.
func speculativeAwaitBudget() time.Duration {
	if v := os.Getenv("SPECULATIVE_AWAIT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 350 * time.Millisecond
}
