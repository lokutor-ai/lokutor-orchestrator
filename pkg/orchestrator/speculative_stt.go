package orchestrator

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Speculative STT: spend the VAD hangover transcribing instead of waiting.
//
// End-of-turn is not declared the moment the caller stops making sound. The
// VAD waits for a run of consecutive silent frames (VAD_HANGOVER_MS, 500ms =>
// 15 frames of 32ms = 480ms in production) to be *sure* the pause is a real
// end of turn rather than a breath. Only then does userSpeechEnd fire, and only
// then did we start the STT pass — a full-utterance Parakeet call measuring a
// median of 444ms on real traffic.
//
// Those two costs are strictly sequential today, and they need not be. The
// hangover is not a period during which more speech arrives: it is a period
// during which we are *waiting to find out* whether more speech arrives. Every
// speech sample of the utterance is already buffered when the first silent
// frame lands. Transcribing the buffer at that point yields the same text as
// transcribing it 480ms later, because the only bytes added in between are the
// silence being measured.
//
// So: kick off the transcription as soon as the hangover starts counting, and
// have the answer in hand by the time end-of-turn actually fires. On a turn
// where the caller really did stop, this removes the whole STT stage from
// time-to-first-audio — the single largest reducible block in the pipeline
// after the LLM's reasoning phase.
//
// The speculation is free to be wrong. If the caller resumes mid-hangover the
// VAD never emits end-of-turn, the buffer keeps growing, and the stale result
// is rejected on a length check (see accept()). The cost of a wrong guess is
// one wasted Parakeet call on a CPU that was otherwise idle waiting; the cost
// of a right guess is ~400ms off every single turn.

const (
	// Start speculating after this many consecutive silent frames.
	//
	// Every frame spent waiting here is a frame the transcription does not get
	// to run inside the hangover, and the hangover is the entire budget this
	// feature has. One frame (32ms) risks firing on a sub-threshold frame
	// mid-word, but that costs one wasted transcription on an idle CPU, while
	// being late costs the caller real milliseconds on a turn that counts.
	// With a shortened hangover the head start matters more, not less.
	defaultSpecSTTSilenceFrames = 1

	// Don't speculate on buffers too short to be a real utterance. Measured at
	// 8000 bytes (~250ms at 16kHz) this skipped short replies — "yes", "that's
	// right" — and those turns paid the full STT wait: 212ms and 351ms on
	// turns that could have paid nothing. Speculation coverage was 7/10.
	//
	// 3200 bytes is ~100ms of speech, below which a buffer is far more likely
	// to be a cough than a word, and the transcription is cheap enough that a
	// wasted one costs nothing worth counting.
	defaultSpecSTTMinBytes = 3200 // ~100ms at 16kHz mono PCM16

	// A speculative result is only usable if the audio appended after the
	// snapshot is plausibly just the rest of the hangover. If the caller
	// resumed speaking, the buffer grows by their new speech plus a fresh
	// hangover, which is far beyond this. Generous enough to absorb hangover
	// jitter, far below the length any resumed speech would add.
	defaultSpecSTTMaxTailMs = 700
)

// specSTT holds one utterance's speculative transcription attempt.
type specSTT struct {
	mu sync.Mutex

	// inFlight is true from launch until the transcription returns.
	inFlight bool
	// done is closed when the result is ready. Nil when nothing is in flight.
	done chan struct{}

	// snapshotLen is len(speechAudioBuf) at the moment we snapshotted. The
	// accept check compares it against the final buffer length.
	snapshotLen int
	// seq is the utterance this attempt belongs to; a newer utterance
	// invalidates it outright.
	seq int

	result TranscriptionResult
	err    error

	// startedAt is when the speculative pass began, for logging how much of
	// the hangover it actually managed to use.
	startedAt time.Time
}

func specSTTEnabled() bool {
	// On by default: this is a strict latency win with a self-correcting
	// failure mode. SPECULATIVE_STT=false is the switch if a caller ever
	// needs the old strictly-sequential behaviour back without a rebuild.
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SPECULATIVE_STT")))
	return v != "false" && v != "0" && v != "off"
}

func specSTTSilenceFrames() int {
	if v := strings.TrimSpace(os.Getenv("SPECULATIVE_STT_SILENCE_FRAMES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultSpecSTTSilenceFrames
}

func specSTTMaxTailMs() int {
	if v := strings.TrimSpace(os.Getenv("SPECULATIVE_STT_MAX_TAIL_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultSpecSTTMaxTailMs
}

// specLLMFromTranscriptEnabled reports whether a completed speculative
// transcription should immediately seed the speculative LLM.
//
// Gated on the speculator existing and SpeculativeLLM being on, so it inherits
// the same switch as every other speculation. SPECULATIVE_STT_CHAIN_LLM=false
// disables just the chaining without disabling either half, which is the knob
// to reach for if a wrong guess ever turns out to cost more than it saves.
func (ms *ManagedStream) specLLMFromTranscriptEnabled() bool {
	if ms.speculator == nil || ms.orch == nil || !ms.orch.config.SpeculativeLLM {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SPECULATIVE_STT_CHAIN_LLM")))
	return v != "false" && v != "0" && v != "off"
}

// silenceFramesProvider is implemented by SileroVAD. Asserted rather than added
// to VADProvider so the RMS fallback and any test double keep working — they
// simply never speculate.
type silenceFramesProvider interface {
	SilenceFrames() int
}

// maybeSpeculateSTT launches a speculative transcription if we have just
// entered the hangover window and nothing is in flight for this utterance.
// Called from the audio loop on every chunk while speech is active; must stay
// cheap on the overwhelming majority of calls where it does nothing.
// The seq is derived here rather than passed in, because getting it from the
// caller is exactly what broke this the first time. utteranceSeq is incremented
// in onVADEnd, so during the hangover it still holds the PREVIOUS utterance's
// number while processUtterance will be handed utteranceSeq+1 — tagging a
// speculation with the current value made awaitUsable compare N-1 against N and
// discard its own result on every turn, silently, with the feature reporting
// itself as enabled the whole time.
func (ms *ManagedStream) maybeSpeculateSTT() {
	if !specSTTEnabled() || ms.vad == nil || ms.orch == nil {
		return
	}
	sf, ok := ms.vad.(silenceFramesProvider)
	if !ok {
		return
	}
	silent := sf.SilenceFrames()
	if silent < specSTTSilenceFrames() {
		// Either mid-speech, or not yet enough silence to believe the turn is
		// ending. Reset any attempt from an earlier pause in the same
		// utterance: the caller carried on, so that snapshot is short by
		// however much they have said since.
		if silent == 0 {
			ms.specSTT.invalidate()
		}
		return
	}

	ms.mu.Lock()
	snapshot := make([]byte, len(ms.speechAudioBuf))
	copy(snapshot, ms.speechAudioBuf)
	lang := ms.session.GetCurrentLanguage()
	// The number this in-flight utterance will carry once onVADEnd commits it.
	seq := ms.utteranceSeq + 1
	ms.mu.Unlock()

	if len(snapshot) < defaultSpecSTTMinBytes {
		return
	}
	if !ms.specSTT.begin(len(snapshot), seq) {
		return // already running (or already finished) for this pause
	}

	go func() {
		// Deliberately not tied to the utterance context: a speculative pass
		// that outlives its usefulness must still finish and release the
		// Parakeet worker slot rather than being abandoned mid-call.
		ctx, cancel := context.WithTimeout(ms.ctx, 10*time.Second)
		defer cancel()
		res, err := ms.orch.Transcribe(ctx, snapshot, lang)
		ms.specSTT.finish(res, err)

		// Hand the transcript straight to the speculative LLM rather than
		// letting it transcribe the same audio again.
		//
		// This is the chain that matters. The hangover is dead time we are
		// already spending; spending it on the LLM as well as the STT means
		// that on a turn the caller has genuinely finished, the response is
		// frequently generated before end-of-turn is even declared — llm_ms
		// goes to zero rather than ~200ms. Without it the speculator ran its
		// own redundant Parakeet pass and started generating strictly later.
		//
		// Guessing wrong costs one LLM call on an otherwise idle CPU: Await
		// returns a response only when its transcript matches the confirmed
		// one, so a guess made on a half-finished sentence is discarded.
		if err != nil || !ms.specLLMFromTranscriptEnabled() {
			return
		}
		text := strings.TrimSpace(res.Text)
		if text == "" {
			return
		}
		ms.speculator.StartFromTranscript(ms.ctx, ms.orch, text,
			ms.session.GetContextCopy(), ms.session.GetTools())
	}()
}

// begin claims the attempt. Returns false if one is already in flight or a
// result is already waiting for this pause.
func (s *specSTT) begin(snapshotLen, seq int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight || s.done != nil {
		return false
	}
	s.inFlight = true
	s.done = make(chan struct{})
	s.snapshotLen = snapshotLen
	s.seq = seq
	s.result = TranscriptionResult{}
	s.err = nil
	s.startedAt = time.Now()
	return true
}

func (s *specSTT) finish(res TranscriptionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inFlight {
		return
	}
	s.inFlight = false
	s.result = res
	s.err = err
	if s.done != nil {
		close(s.done)
	}
}

// invalidate drops any attempt. The in-flight goroutine is left to complete
// and its result discarded — cancelling it would not return the CPU any sooner
// and would complicate the Parakeet worker's single-flight accounting.
func (s *specSTT) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		// Mark it orphaned: finish() will no-op, and the waiter sees no done
		// channel. Clearing `done` here is what makes a later begin() legal.
		s.inFlight = false
		if s.done != nil {
			close(s.done)
		}
	}
	s.done = nil
	s.snapshotLen = 0
	s.seq = -1
	s.result = TranscriptionResult{}
	s.err = nil
}

// awaitUsable returns a speculative transcript for this utterance if one is
// valid, waiting briefly for an in-flight pass to land.
//
// finalLen is the length of the audio actually being transcribed. The
// speculative snapshot is usable only when the difference is small enough to be
// the tail of the hangover rather than speech the caller added afterwards.
func (s *specSTT) awaitUsable(ctx context.Context, seq, finalLen, bytesPerMs, maxTailMs int) (TranscriptionResult, bool, time.Duration) {
	s.mu.Lock()
	done := s.done
	snapLen := s.snapshotLen
	mySeq := s.seq
	startedAt := s.startedAt
	s.mu.Unlock()

	if done == nil || mySeq != seq {
		return TranscriptionResult{}, false, 0
	}
	// Reject before waiting: if the caller resumed, waiting buys nothing.
	if bytesPerMs > 0 {
		tailMs := (finalLen - snapLen) / bytesPerMs
		if tailMs < 0 || tailMs > maxTailMs {
			return TranscriptionResult{}, false, 0
		}
	}

	waitStart := time.Now()
	select {
	case <-done:
	case <-ctx.Done():
		return TranscriptionResult{}, false, time.Since(waitStart)
	}

	s.mu.Lock()
	res, err, resSeq := s.result, s.err, s.seq
	s.mu.Unlock()

	if err != nil || resSeq != seq || strings.TrimSpace(res.Text) == "" {
		return TranscriptionResult{}, false, time.Since(waitStart)
	}
	_ = startedAt
	return res, true, time.Since(waitStart)
}
