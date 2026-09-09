package orchestrator

import (
	"context"
	"math"
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
	if ms.speculator == nil || ms.orch == nil || !ms.orch.config.SpeculativeLLM {
		return false
	}

	// Bounded wait for an in-flight run: if speculation started early, most
	// of its generation time has already elapsed in parallel with the real
	// STT round trip by the time we get here, so this rarely adds much —
	// but cap it so an unusually slow speculative run can never make a
	// miss slower than just running the normal path would have been.
	awaitCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	response, ok := ms.speculator.Await(awaitCtx, transcript)
	cancel()
	// Always clean up: Await leaves the executor in SpecReady/SpecRunning,
	// and ShouldSpeculate/ShouldSpeculateOnPause both require SpecIdle to
	// start a new run — without this, one used-or-missed speculation would
	// permanently block every later attempt for the rest of the call.
	ms.speculator.Cancel()

	if !ok {
		return false
	}

	rCtx, rCancel := context.WithCancel(ctx)
	ms.mu.Lock()
	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
	}
	ms.pipelineCancel = rCancel
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()
	defer rCancel()

	ms.emitWithGen(BotThinking, nil, gen)
	ms.llmStartTime = time.Now()
	ms.llmEndTime = time.Now() // already generated ahead of time — no LLM wait on this turn

	ms.mu.Lock()
	ms.lastResponseText = response
	ms.spokenTextPrefix = ""
	ms.spokenTextLocked = false
	ms.mu.Unlock()
	ms.session.AddMessage("assistant", response)
	ms.emitWithGen(BotResponse, response, gen)
	ms.cacheResponse(transcript, response, nil)

	ms.logger.Info("Speculative LLM response used", "transcript", transcript)
	ms.speakText(rCtx, response, gen)
	return true
}
