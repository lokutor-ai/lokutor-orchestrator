package orchestrator

import (
	"context"
	"math"
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

	// lastVoiced is the last frame the VAD heard voice on (noteVoiced). A voiced frame after startedAt means the
	// caller said more after the snapshot: "I'd like to book an appointment" [pause] "for" arrived as the snapshot's
	// "...an appointment." because "for" plus the hangover fit in maxTailMs, and the turn lost its open ending.
	lastVoiced time.Time
	// voicedAfter counts voiced frames heard after startedAt. specVoicedFramesAfter or more is speech (a short word
	// is ~3 VAD frames); fewer is the VAD flickering on a word's trailing edge, which must not throw away the
	// snapshot's head start.
	voicedAfter int
	// speechPeakDb is the loudest 20 ms of the snapshot's speech (dBFS; 0 until setSpeechPeak), and loudMsAfter how
	// much audio after startedAt came within specLoudBelowPeakDb of it. The VAD alone misses a soft last word: on a
	// rendered "I'd like to book an appointment for", Silero scored "for" 0.036-0.040 against a 0.065 threshold while
	// it was 10 dB under the sentence's peak, so no frame after the snapshot counted as voiced and the turn arrived as
	// "...an appointment." (2026-10-22).
	speechPeakDb float64
	loudMsAfter  int

	// cancel stops the pass in flight. invalidate() calls it: the result would be discarded anyway, and a pass
	// still denoising skips its recognition request (2026-10-16).
	cancel context.CancelFunc

	// attempt numbers the passes begun. A pass finishes only its own attempt: invalidate() lets the
	// next pause begin another while the old pass is still transcribing, and the old pass landing
	// then was taken as the new attempt's result. That transcript is short of the words said between
	// the two pauses, and the length check at end of turn measures the new snapshot, so it was
	// accepted and the turn answered without them. The old pass also seeded the model with its stale
	// words, a paid run that could only be thrown away.
	attempt int
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

// hangoverSeedActive reports whether guesses come from the hangover seed: speculative STT on, chained
// into the LLM, and a VAD that can say how long the silence has been (the seed's trigger). When it is,
// the audio-triggered guesses stay off; when it is not (an RMS VAD, or the chain disabled), they are
// still the only speculation there is.
func (ms *ManagedStream) hangoverSeedActive() bool {
	if !specSTTEnabled() || !ms.specLLMFromTranscriptEnabled() || ms.vad == nil {
		return false
	}
	_, ok := ms.vad.(silenceFramesProvider)
	return ok
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
			// The caller carried on talking, so any guess seeded from the pause is for words they
			// have since added to: stop paying for it (the next pause seeds a fresh one).
			if ms.speculator != nil && ms.speculator.State() != SpecIdle {
				ms.speculator.Cancel()
			}
		}
		return
	}

	// The snapshot carries the same ~300ms lead-in onVADStart gives userAudio, for the same reason:
	// the VAD confirms speech a few frames after it starts, so the frames it appended from begin
	// part-way into the first word. Without it this path — which answers ~96% of production turns —
	// transcribed every utterance minus its onset, while the batch path it replaces had it.
	// Measured on the deploy smoke stimulus ("Hola, ¿a qué hora abre la oficina mañana?"), cut where
	// production's VAD actually cut it: the two segments transcribe as "Yeah." and "Abre la oficina
	// mañana." without the lead-in — exactly what production heard, 68 of 68 sessions on 2026-09-23 —
	// and as "Hola." and "¿Qué hora abre la oficina mañana?" with it. A one-word answer loses the most:
	// on 2026-09-24 a caller's name, ~430 ms, came back "Yeah." and "Okay." with its onset cut off.
	//
	// It also puts both sides of awaitUsable's tail check on the same footing: finalLen is
	// len(userAudio), which includes the lead-in, so a snapshot without it read every tail ~300ms
	// longer than the audio actually appended after it.
	ms.mu.Lock()
	speechLen := len(ms.speechAudioBuf)
	snapshot := make([]byte, 0, len(ms.speechLeadIn)+speechLen)
	snapshot = append(snapshot, ms.speechLeadIn...)
	snapshot = append(snapshot, ms.speechAudioBuf...)
	lang := ms.session.GetCurrentLanguage()
	// The number this in-flight utterance will carry once onVADEnd commits it.
	seq := ms.utteranceSeq + 1
	ms.mu.Unlock()

	// Measured on the speech alone: the lead-in would otherwise clear this on any blip.
	if speechLen < defaultSpecSTTMinBytes {
		return
	}
	attempt, ok := ms.specSTT.start(len(snapshot), seq)
	if !ok {
		return // already running (or already finished) for this pause
	}
	if rate := int(ms.inputSampleRate); rate > 0 {
		ms.specSTT.setSpeechPeak(peakFrameDbfs(snapshot[len(snapshot)-speechLen:], rate/50*2))
	}

	go func() {
		// Not tied to the utterance context: the pass belongs to this pause, and invalidate() (the caller
		// carried on, or a new utterance began) is what stops it, through setCancel.
		ctx, cancel := context.WithTimeout(ms.ctx, 10*time.Second)
		defer cancel()
		ms.specSTT.setCancel(attempt, cancel)
		res, err := ms.orch.Transcribe(ctx, snapshot, lang)
		if ok, displaced := ms.specSTT.finishAttempt(attempt, res, err); !ok {
			// The caller carried on after this pause: the transcript is short of what they said
			// since, and neither the turn nor a guess may use it. Logged when a later pause's pass
			// was running, the case that used to take this transcript for the turn's.
			if displaced && err == nil {
				ms.logger.Info("Speculative STT: an earlier pause's transcript finished during a later pause's pass and was discarded",
					"attempt", attempt, "text", ContentForLog(res.Text))
			}
			return
		}

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
		// Logged because the win is invisible otherwise: if this never starts,
		// or starts and is never matched at end-of-turn, llm_ms simply stays at
		// its normal value and nothing says why.
		before := ms.speculator.State()
		seed, history, continues := ms.speculationSeed(text, seq)
		history, tail := ms.seedKnowledgeRequest(ms.ctx, seed, history)
		ms.speculator.StartFromTranscriptWithTail(ms.ctx, ms.orch, seed, history, tail, ms.session.GetTools())
		ms.logger.Info("Speculative LLM seeded from hangover transcript", "run", ms.speculator.RunID(),
			"transcript", ContentForLog(seed), "continues", continues, "state_before", before, "state_after", ms.speculator.State())
	}()
}

// speculationSeed is what the hangover seed asks the model: the words, and the context, that this
// utterance's turn will carry once processUtterance has joined it to the utterance it continues
// (spoken_truth.go). continues is that earlier utterance's transcript, or "".
//
// Seeded with the fragment alone, a joined turn never matched its guess (Await compares the run's
// transcript with the turn's) and paid a whole completion for nothing: 15% of turns were joined, and
// they used a speculative reply on 2.2% against 11.8% for the rest (14 days of /lokutor/system to
// 2026-10-01).
//
//   - A carried fragment is joined as text unless a joint transcription lands in time and is accepted,
//     usually as the same words. It never reached the context, so the context is unchanged.
//   - Over a reply, a previous utterance that settled on its own ("Hola.") is joined as text and the
//     reply it got leaves the context, so the guess is asked against the context without either. When
//     anything other than replies follows that utterance (a note, a tool call) the turn would be
//     built differently, and the fragment is asked as before.
//   - Over a reply, an unsettled utterance joins only through a joint transcription (unknowable here)
//     or stays a turn of its own; the fragment is the guess for the second.
func (ms *ManagedStream) speculationSeed(text string, seq int) (seed string, history []Message, continues string) {
	history = ms.session.GetContextCopy()
	now := time.Now()
	ms.mu.Lock()
	carry := ms.carry
	if carry != nil && now.Sub(carry.endedAt) > continuationMergeWindow {
		carry = nil
	}
	var barge *committedUtterance
	if carry == nil {
		barge = ms.bargeBaseLocked(now, seq, ms.pendingBargeIn && ms.pendingBargeGen == ms.payloadGen)
	}
	ms.mu.Unlock()

	switch {
	case carry != nil:
		return spaceSentences(strings.TrimSpace(carry.transcript + " " + text)), history, carry.transcript
	case barge != nil && !needsJointPass(barge.transcript):
		merged := spaceSentences(strings.TrimSpace(barge.transcript + " " + text))
		if revised, ok := replaceLastUserTurn(history, merged); ok {
			if last := revised[len(revised)-1]; last.Role == "user" && last.Content == merged {
				return merged, revised[:len(revised)-1], barge.transcript
			}
		}
	}
	return text, history, ""
}

// begin claims the attempt. Returns false if one is already in flight or a
// result is already waiting for this pause.
func (s *specSTT) begin(snapshotLen, seq int) bool {
	_, ok := s.start(snapshotLen, seq)
	return ok
}

// start is begin, also returning the attempt's number for finishAttempt.
func (s *specSTT) start(snapshotLen, seq int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight || s.done != nil {
		return 0, false
	}
	s.attempt++
	s.inFlight = true
	s.done = make(chan struct{})
	s.snapshotLen = snapshotLen
	s.seq = seq
	s.result = TranscriptionResult{}
	s.err = nil
	s.startedAt = time.Now()
	s.voicedAfter = 0
	s.speechPeakDb = 0
	s.loudMsAfter = 0
	s.cancel = nil
	return s.attempt, true
}

// noteVoiced records a frame the VAD heard voice on.
func (s *specSTT) noteVoiced(t time.Time) {
	s.mu.Lock()
	s.lastVoiced = t
	if !s.startedAt.IsZero() && t.After(s.startedAt) {
		s.voicedAfter++
	}
	s.mu.Unlock()
}

const specVoicedFramesAfter = 3

const (
	// Audio within this much of the snapshot's loudest 20 ms is speech, not the hangover's silence: a soft function
	// word sits ~10 dB under the peak, a room or line's floor usually 30 dB or more.
	specLoudBelowPeakDb = 20.0
	// specLoudMsAfter of it after the snapshot is a word (a short one is ~60 ms of loud frames), less a click.
	specLoudMsAfter = 60
)

// setSpeechPeak records the snapshot's speech peak for noteAudio.
func (s *specSTT) setSpeechPeak(db float64) {
	s.mu.Lock()
	s.speechPeakDb = db
	s.mu.Unlock()
}

// noteAudio measures a 16 kHz frame heard after the snapshot against its speech peak.
func (s *specSTT) noteAudio(pcm16k []byte, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startedAt.IsZero() || !t.After(s.startedAt) || s.speechPeakDb == 0 || len(pcm16k) < 2 {
		return
	}
	if rmsDbfs(pcm16k) >= s.speechPeakDb-specLoudBelowPeakDb {
		s.loudMsAfter += len(pcm16k) / 32
	}
}

// heardAfter is what was heard after the snapshot: voiced VAD frames and loud milliseconds.
func (s *specSTT) heardAfter() (voiced, loudMs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voicedAfter, s.loudMsAfter
}

// rmsDbfs is the RMS level of 16-bit PCM in dBFS (-200 for digital silence).
func rmsDbfs(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return -200
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(pcm[2*i])|uint16(pcm[2*i+1])<<8)) / 32768
		sum += v * v
	}
	if sum == 0 {
		return -200
	}
	return 10 * math.Log10(sum/float64(n))
}

// peakFrameDbfs is the loudest frameBytes-long window of pcm in dBFS.
func peakFrameDbfs(pcm []byte, frameBytes int) float64 {
	peak := -200.0
	if frameBytes < 2 {
		return peak
	}
	frameBytes &^= 1
	for off := 0; off+frameBytes <= len(pcm); off += frameBytes {
		if db := rmsDbfs(pcm[off : off+frameBytes]); db > peak {
			peak = db
		}
	}
	return peak
}

// setCancel registers how to stop attempt's pass. An attempt already invalidated is stopped at once.
func (s *specSTT) setCancel(attempt int, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight && s.attempt == attempt {
		s.cancel = cancel
		return
	}
	cancel()
}

// finish records the result of the attempt in flight.
func (s *specSTT) finish(res TranscriptionResult, err error) {
	s.mu.Lock()
	attempt := s.attempt
	s.mu.Unlock()
	s.finishAttempt(attempt, res, err)
}

// finishAttempt records attempt's result if it is still the attempt in flight, and reports whether it
// was. An orphaned pass (invalidated, and perhaps replaced by a later pause's) changes nothing;
// displaced says a later attempt was in flight, the case that used to take the orphan's result.
func (s *specSTT) finishAttempt(attempt int, res TranscriptionResult, err error) (ok, displaced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inFlight || s.attempt != attempt {
		return false, s.inFlight && s.attempt != attempt
	}
	s.inFlight = false
	s.cancel = nil
	s.result = res
	s.err = err
	if s.done != nil {
		close(s.done)
	}
	return true, false
}

// invalidate drops any attempt, and cancels the pass in flight: its result would be discarded. It used to be left
// to finish ("cancelling would not return the CPU any sooner and would complicate the Parakeet worker's single-flight
// accounting"); since 09-29 the denoiser honours cancellation, so a pass still denoising stops there and never sends
// its recognition request, and the recognition client returns on cancellation without tripping its breaker.
func (s *specSTT) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
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

	// Reject when the caller spoke after the snapshot: its transcript is short of what they said.
	s.mu.Lock()
	spokeAfter := s.voicedAfter >= specVoicedFramesAfter || s.loudMsAfter >= specLoudMsAfter
	s.mu.Unlock()
	if spokeAfter {
		return TranscriptionResult{}, false, 0
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
