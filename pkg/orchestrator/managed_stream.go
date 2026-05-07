package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ManagedStream struct {
	orch    *Orchestrator
	session *ConversationSession
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan OrchestratorEvent
	vad     VADProvider

	audioBuf *bytes.Buffer
	mu       sync.Mutex

	pipelineCtx         context.Context
	pipelineCancel      context.CancelFunc
	sttChan             chan<- []byte
	sttGeneration       int
	isSpeaking          bool
	isThinking          bool
	lastAudioSentAt     time.Time
	userSpeechStartTime time.Time
	userSpeechEndTime   time.Time
	botSpeakStartTime   time.Time

	lastUserAudio  []byte
	lastTranscript string
	turnCompletion *TurnCompletionAnalyzer

	sttStartTime        time.Time
	sttRequestStartTime time.Time
	sttEndTime          time.Time
	llmStartTime        time.Time
	llmEndTime          time.Time
	ttsStartTime        time.Time
	ttsFirstChunkTime   time.Time
	ttsEndTime          time.Time

	responseCancel   context.CancelFunc
	ttsCancel        context.CancelFunc
	userInterrupting bool
	echoSuppressor   *EchoSuppressor
	closeOnce        sync.Once

	vadDebounceUntil time.Time

	payloadGen       int
	writeChan        chan []byte
	isClosed         bool
	lastNoSpeechProb float64
	inPreemptiveTurn bool
	lastActivityAt   time.Time
	playbackRate     int

	toolRecursionDepth int

	sentenceBuffer *SentenceBuffer
	backchannelGen *BackchannelGenerator
	speculator     *Speculator

	ttsSentenceChan chan string

	userSegmentRMS     float64
	userSegmentSamples int
	userBaselineRMS    float64
	segmentRMSStart    time.Time

	turnCount int

	config Config
}

func NewManagedStream(ctx context.Context, o *Orchestrator, session *ConversationSession) *ManagedStream {
	mCtx, mCancel := context.WithCancel(ctx)

	var streamVAD VADProvider
	if o != nil && o.vad != nil {
		streamVAD = o.vad.Clone()
	}

	config := DefaultConfig()
	if o != nil {
		config = o.GetConfig()
	}

	ms := &ManagedStream{
		orch:           o,
		session:        session,
		ctx:            mCtx,
		cancel:         mCancel,
		events:         make(chan OrchestratorEvent, 1024),
		audioBuf:       new(bytes.Buffer),
		vad:            streamVAD,
		echoSuppressor: NewEchoSuppressorWithConfig(config),
		writeChan:      make(chan []byte, 512),
		lastActivityAt: time.Now(),
		playbackRate:   44100,
		turnCompletion: NewTurnCompletionAnalyzer(),
		sentenceBuffer: NewSentenceBuffer(),
		config:         config,
	}

	ms.speculator = NewSpeculator(o, session, config.SpeculativeEnabled)
	if config.SpeculativeEnabled && o != nil && o.llm != nil {
		ms.speculator.SetSpeculativeProvider(o.llm)
	}
	ms.backchannelGen = NewBackchannelGenerator(o, session, config.BackchannelEnabled, config.BackchannelThreshold, config.Language)

	ms.ttsSentenceChan = make(chan string, 32)

	go ms.processBackgroundAudio()
	go ms.monitorInactivity()

	if config.BackchannelEnabled {
		go ms.backchannelGen.PreWarm(mCtx)
	}

	if o != nil && o.config.FirstSpeaker == FirstSpeakerBot {
		go func() {
			time.Sleep(500 * time.Millisecond)
			greeting := "Hello!"
			if o.config.Language == LanguageEs {
				greeting = "¡Hola!"
			}
			ms.session.AddMessage("assistant", greeting)
			ms.runLLMAndTTS(ms.ctx, greeting)
		}()
	}

	return ms
}

func (ms *ManagedStream) processBackgroundAudio() {
	for {
		select {
		case <-ms.ctx.Done():
			return
		case chunk := <-ms.writeChan:
			ms.doWrite(chunk)
		}
	}
}

func (ms *ManagedStream) LastRMS() float64 {
	if ms.vad == nil {
		return 0.0
	}
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		return rmsVAD.LastRMS()
	}
	return 0.0
}

func (ms *ManagedStream) IsUserSpeaking() bool {
	if ms.vad == nil {
		return false
	}
	return ms.vad.IsSpeaking()
}

func (ms *ManagedStream) SetEchoSampleRates(playbackRate, inputRate int) {
	ms.mu.Lock()
	ms.playbackRate = playbackRate
	ms.mu.Unlock()
	if ms.echoSuppressor != nil {
		ms.echoSuppressor.SetSampleRates(playbackRate, inputRate)
	}
}

func (ms *ManagedStream) Interrupt() {
	ms.mu.Lock()
	ms.userInterrupting = true
	ms.mu.Unlock()
	ms.internalInterrupt()
}

func countWords(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Fields(s))
}

func (ms *ManagedStream) Write(chunk []byte) error {
	// We MUST copy the chunk here because the caller (main.go) will recycle the
	// underlying buffer into the sync.Pool as soon as this function returns.
	// Without this copy, doWrite() would be processing memory that is being
	// simultaneously overwritten by the microphone callback.
	buf := make([]byte, len(chunk))
	copy(buf, chunk)

	ms.writeChan <- buf
	return nil
}

func (ms *ManagedStream) computeRMS(chunk []byte) float64 {
	if len(chunk) < 2 {
		return 0
	}
	var sum float64
	n := 0
	for i := 0; i < len(chunk)-1; i += 2 {
		sample := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		f := float64(sample) / 32768.0
		sum += f * f
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

func (ms *ManagedStream) doWrite(chunk []byte) error {
	ms.mu.Lock()
	if ms.ctx.Err() != nil {
		ms.mu.Unlock()
		return ms.ctx.Err()
	}
	ms.mu.Unlock()

	if ms.vad == nil {
		return fmt.Errorf("VAD not configured for this stream")
	}

	vadChunk := chunk
	if ms.echoSuppressor != nil {
		vadChunk = ms.echoSuppressor.RemoveEchoRealtime(chunk)
	}

	rms := ms.computeRMS(vadChunk)
	if ms.backchannelGen != nil {
		ms.backchannelGen.RecordAudio(rms)
	}

	ms.mu.Lock()
	userSpeaking := false
	if ms.vad != nil {
		userSpeaking = ms.vad.IsSpeaking()
	}
	if userSpeaking && rms > 0.001 {
		ms.userSegmentRMS += rms
		ms.userSegmentSamples++
		ms.userBaselineRMS = ms.userBaselineRMS*0.995 + rms*0.005
	} else if !userSpeaking {
		ms.userSegmentRMS = 0
		ms.userSegmentSamples = 0
	}
	ms.mu.Unlock()

	event, err := ms.vad.Process(vadChunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
		switch event.Type {
		case VADSpeechPotential:

		case VADSpeechStart:
			if ms.backchannelGen != nil {
				ms.backchannelGen.RecordUserSpeechStart()
			}

			ms.mu.Lock()
			if ms.userSpeechStartTime.IsZero() {
				ms.userSpeechStartTime = time.Now()
			}
			ms.segmentRMSStart = time.Now()
			ms.userSegmentRMS = 0
			ms.userSegmentSamples = 0

			ms.sttStartTime = time.Now()
			ms.sttRequestStartTime = time.Time{}
			ms.sttEndTime = time.Time{}
			ms.llmStartTime = time.Time{}
			ms.llmEndTime = time.Time{}
			ms.ttsStartTime = time.Time{}
			ms.ttsFirstChunkTime = time.Time{}
			ms.ttsEndTime = time.Time{}
			ms.botSpeakStartTime = time.Time{}
			ms.lastAudioSentAt = time.Time{}

			speaking := ms.isSpeaking
			ms.mu.Unlock()

			ms.emit(UserSpeaking, nil)

			ms.mu.Lock()
			ms.sttGeneration++
			sttChan := ms.sttChan
			ms.sttChan = nil
			ms.mu.Unlock()

			if sttChan != nil {
				close(sttChan)
			}

			ms.speculator.Reset()
			ms.sentenceBuffer.Reset()

			if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
				if speaking {
					ms.mu.Lock()
					ms.vadDebounceUntil = time.Now().Add(300 * time.Millisecond)
					ms.mu.Unlock()
					go ms.debounceThenStartSTT(sProvider)
				} else {
					ms.emit(UserSpeaking, nil)
					ms.startStreamingSTT(sProvider)
				}
			}
		case VADSpeechEnd:
			if ms.backchannelGen != nil {
				ms.backchannelGen.OnSpeechEnd()
			}

			ms.mu.Lock()
			ms.userSpeechEndTime = time.Now()
			ms.mu.Unlock()
			ms.emit(UserStopped, nil)

			ms.mu.Lock()
			sttChan := ms.sttChan
			if sttChan != nil {
				ms.sttChan = nil
				ms.mu.Unlock()
				close(sttChan)
			} else {
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.mu.Unlock()

				go func(buf []byte) {
					ms.mu.Lock()
					duration := ms.userSpeechEndTime.Sub(ms.userSpeechStartTime)
					lastTranscript := ms.lastTranscript
					ms.mu.Unlock()

					if duration < 500*time.Millisecond {
						ms.runBatchPipeline(buf)
						return
					}

					completionScore := ms.turnCompletion.CombinedCompletionScore(
						lastTranscript,
						int(duration.Milliseconds()),
						ms.vad,
					)

					acousticComplete := ms.acousticCompletionHint()

					var holdTime time.Duration
					if completionScore < 0.35 && !acousticComplete {
						holdTime = 600 * time.Millisecond
					} else if completionScore > 0.65 || acousticComplete {
						holdTime = 50 * time.Millisecond
					} else {
						if duration < 1500*time.Millisecond {
							holdTime = 350 * time.Millisecond
						} else {
							holdTime = 200 * time.Millisecond
						}
					}

					t := time.NewTimer(holdTime)
					defer t.Stop()

					select {
					case <-t.C:
						if ms.vad != nil && ms.vad.IsSpeaking() {
							return
						}
						ms.runBatchPipeline(buf)
					case <-ms.ctx.Done():
						return
					}
				}(audioData)
			}

		case VADSilence:
		}
	}

	isUserSpeaking := ms.vad.IsSpeaking()
	if isUserSpeaking {
		ms.updateActivity()
		if ms.backchannelGen != nil {
			ms.backchannelGen.OnUserResumed()
		}
	} else {
		if ms.backchannelGen != nil && event != nil && event.Type == VADSilence {
			ms.mu.Lock()
			speaking := ms.isSpeaking
			thinking := ms.isThinking
			userStart := ms.userSpeechStartTime
			ms.mu.Unlock()

			if !speaking && !thinking && !userStart.IsZero() && time.Since(userStart) > time.Second {
				doBC, bcText := ms.backchannelGen.OnUserPause()
				if doBC && bcText != "" {
					go func(t string) {
						audio, err := ms.backchannelGen.SynthesiszeBackchannel(ms.ctx, t)
						if err != nil || len(audio) == 0 {
							return
						}
						ms.mu.Lock()
						gen := ms.payloadGen
						ms.mu.Unlock()
						frameSize := int(float64(ms.playbackRate)*0.06) * 2
						if frameSize <= 0 {
							frameSize = 5292
						}
						for i := 0; i < len(audio); i += frameSize {
							end := i + frameSize
							if end > len(audio) {
								end = len(audio)
							}
							c := make([]byte, end-i)
							copy(c, audio[i:end])
							ms.emitWithGen(AudioChunk, c, gen)
						}
					}(bcText)
				}
			}
		}
	}

	cleanChunk := chunk
	if len(cleanChunk)%2 != 0 {
		cleanChunk = cleanChunk[:len(cleanChunk)-1]
	}

	ms.mu.Lock()
	ms.audioBuf.Write(cleanChunk)
	if !isUserSpeaking && ms.userSpeechStartTime.IsZero() && ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-132300:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}
	ms.mu.Unlock()

	ms.mu.Lock()
	sttChan := ms.sttChan
	ms.lastUserAudio = append(ms.lastUserAudio, cleanChunk...)
	ms.mu.Unlock()

	if sttChan != nil {
		toSend := make([]byte, len(cleanChunk))
		copy(toSend, cleanChunk)

		// VAD Watchdog: If we've been transcribing for more than 15s without a VADSpeechEnd,
		// force a commit to prevent getting stuck in noise.
		ms.mu.Lock()
		startTime := ms.userSpeechStartTime
		ms.mu.Unlock()
		if !startTime.IsZero() && time.Since(startTime) > 15*time.Second {
			fmt.Printf("\r\033[K[DEBUG] VAD Watchdog fired (15s speech segment). Forcing speech end.\n")
			ms.mu.Lock()
			ms.userSpeechEndTime = time.Now()
			ms.sttChan = nil
			ms.mu.Unlock()
			close(sttChan)
			return nil
		}

		select {
		case sttChan <- toSend:
		default:
		}
	}

	return nil
}

func (ms *ManagedStream) isLikelyNoise(result TranscriptionResult, audioDuration time.Duration, speaking bool) bool {
	if result.NoSpeechProb > 0.7 {
		return true
	}

	clean := strings.TrimSpace(result.Text)
	if clean == "" {
		return true
	}

	if len(clean) <= 1 || audioDuration < 150*time.Millisecond {
		return true
	}
	if speaking && len(clean) <= 2 && audioDuration < 500*time.Millisecond {
		return true
	}
	return false
}

func (ms *ManagedStream) acousticCompletionHint() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.userSegmentSamples == 0 {
		return false
	}

	avgRMS := ms.userSegmentRMS / float64(ms.userSegmentSamples)
	baseline := ms.userBaselineRMS
	if baseline <= 0 {
		baseline = 0.02
	}

	energyRatio := avgRMS / baseline

	fallingEnergy := energyRatio < 0.8
	shortUtterance := ms.userSegmentSamples < 20

	return fallingEnergy && shortUtterance
}

func (ms *ManagedStream) debounceThenStartSTT(provider StreamingSTTProvider) {
	// Wait for debounce period to filter out brief noises (door slam, clap, cough)
	ms.mu.Lock()
	deadline := ms.vadDebounceUntil
	ms.mu.Unlock()
	time.Sleep(time.Until(deadline))

	ms.mu.Lock()
	// Check if VAD is still speaking (user actually continued past debounce window)
	vadSpeaking := ms.vad != nil && ms.vad.IsSpeaking()
	// Check if bot is still speaking (hasn't already been interrupted)
	botSpeaking := ms.isSpeaking
	ms.mu.Unlock()

	if vadSpeaking && botSpeaking {
		ms.emit(UserSpeaking, nil)
		ms.startStreamingSTT(provider)
	} else if !vadSpeaking {
		// Brief noise - VAD stopped before debounce expired
		fmt.Printf("\r\033[K🔇 [VAD-DEBOUNCE] Ignored brief VAD spike (noise)\n")
	}
}

func (ms *ManagedStream) startStreamingSTT(provider StreamingSTTProvider) {

	ctx, cancel := context.WithCancel(ms.ctx)

	ms.mu.Lock()
	currentGeneration := ms.sttGeneration
	ms.mu.Unlock()

	sttChan, err := provider.StreamTranscribe(ctx, ms.session.GetCurrentLanguage(), func(transcript string, isFinal bool) error {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		thinking := ms.isThinking
		isStale := ms.sttGeneration != currentGeneration
		ms.lastTranscript = transcript
		segmentRMS := ms.userSegmentRMS
		segmentSamples := ms.userSegmentSamples
		baselineRMS := ms.userBaselineRMS
		segmentDuration := time.Since(ms.segmentRMSStart)
		ms.mu.Unlock()

		if isStale && !isFinal {
			return nil
		}

		avgSegmentRMS := 0.0
		if segmentSamples > 0 {
			avgSegmentRMS = segmentRMS / float64(segmentSamples)
		}

		ms.mu.Lock()
		minWords := 1
		if ms.orch != nil {
			minWords = ms.orch.GetConfig().MinWordsToInterrupt
		}
		duration := time.Since(ms.sttStartTime)
		ms.mu.Unlock()

		if speaking || thinking {
			if ms.speculator != nil && ms.config.SpeculativeEnabled {
				ms.speculator.OnInterimTranscript(ms.ctx, transcript)
			}

			isBackchannel := false
			if segmentSamples > 0 {
				isBackchannel = IsLikelyBackchannelAcoustic(transcript, avgSegmentRMS, baselineRMS, segmentDuration, ms.config.AcousticInterruptThreshold)
			}

			if isBackchannel && !isFinal {
				ms.emit(TranscriptPartial, transcript)
				return nil
			}

			wc := countWords(transcript)
			shouldInterrupt := false

			// Skip interrupt if this is likely echo from the bot's own TTS.
			// Only check if bot was speaking and transcript has high word overlap
			// with last assistant response (echo signature).
			isLikelyEcho := false
			if speaking && ms.isEchoTranscript(transcript) {
				isLikelyEcho = true
				fmt.Printf("\r\033[K🌀 [ECHO-GUARD] Skipping interrupt, overlap detected\n")
			}

			if minWords > 1 {
				if wc >= minWords && !isLikelyEcho {
					shouldInterrupt = true
				}
			} else {
				if strings.TrimSpace(transcript) != "" && !isLikelyEcho {
					shouldInterrupt = true
				}
			}

			if shouldInterrupt {
				noise := ms.isLikelyNoise(TranscriptionResult{Text: transcript}, duration, speaking)
				if !noise && !isBackchannel {
					ms.internalInterrupt()
				}
			}
		}

		if isFinal {
			ms.mu.Lock()
			ms.sttEndTime = time.Now()
			duration := time.Since(ms.sttStartTime)
			ms.mu.Unlock()

			if ms.isLikelyNoise(TranscriptionResult{Text: transcript}, duration, speaking) {
				fmt.Printf("\r\033[K🔄 [NOISE] Rejected hallucination: '%s' (dur=%v)\n", transcript, duration)
				ms.emit(BotResumed, nil)
				return nil
			}

			// Echo guard: if the bot was speaking when this audio was captured,
			// the transcription is likely echo from TTS playback. Skip processing
			// it as user input to prevent an interrupt loop.
			if speaking && ms.isEchoTranscript(transcript) {
				fmt.Printf("\r\033[K🌀 [ECHO] Skipped echo transcript: '%s'\n", transcript)
				return nil
			}

			acceptedSpeculation := false
			if ms.speculator != nil && ms.config.SpeculativeEnabled {
				candidate := ms.speculator.OnFinalTranscript(ms.ctx, transcript)
				if candidate != nil && candidate.acceptOnFinal {
					acceptedSpeculation = true
					sentence, audio := ms.speculator.AcceptAndConsume()
					if sentence != "" && len(audio) > 0 {
						ms.mu.Lock()
						gen := ms.payloadGen
						ms.session.AddMessage("user", transcript)
						ms.inPreemptiveTurn = true
						ms.session.AddMessage("assistant", sentence)
						ms.mu.Unlock()

						ms.emit(TranscriptFinal, transcript)
						ms.emitWithGen(BotResponse, sentence, gen)

						ms.mu.Lock()
						ms.isSpeaking = true
						ms.isThinking = false
						if ms.vad != nil {
							ms.vad.Reset()
						}
						ms.botSpeakStartTime = time.Now()
						ms.mu.Unlock()

						ms.emit(BotSpeaking, nil)
						frameSize := int(float64(ms.playbackRate)*0.06) * 2
						if frameSize <= 0 {
							frameSize = 5292
						}
						for i := 0; i < len(audio); i += frameSize {
							end := i + frameSize
							if end > len(audio) {
								end = len(audio)
							}
							c := make([]byte, end-i)
							copy(c, audio[i:end])
							ms.emitWithGen(AudioChunk, c, gen)
						}

						ms.mu.Lock()
						ms.isSpeaking = false
						ms.mu.Unlock()

						// Turn is complete: user message + assistant response
						// already in context, audio already played. Do NOT
						// re-run the LLM — that would double the first sentence.
						return nil
					}
				}
			}

			if !acceptedSpeculation {
				ms.emit(TranscriptFinal, transcript)
				ms.mu.Lock()
				if ms.inPreemptiveTurn {
					ms.mu.Unlock()
					ms.session.UpdateLastUserMessage(transcript)
				} else {
					ms.inPreemptiveTurn = true
					ms.mu.Unlock()
					ms.session.AddMessage("user", transcript)
				}

				go ms.runLLMAndTTS(ctx, transcript)
			}
		} else {
			ms.emit(TranscriptPartial, transcript)
		}
		return nil
	})

	if err != nil {
		// Just log or emit a warning, do not cancel the whole pipeline
		// because the orchestrator will gracefully fall back to batch Transcribe.
		fmt.Printf("Warning: could not start streaming STT (falling back to batch): %v\n", err)
		ms.mu.Lock()
		ms.pipelineCtx = ctx
		ms.pipelineCancel = cancel
		ms.sttChan = nil
		ms.sttStartTime = time.Now()
		ms.mu.Unlock()
		return
	}

	ms.mu.Lock()
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttChan = sttChan
	ms.sttStartTime = time.Now()

	// Flush pre-buffered audio to STT channel with blocking send
	// This ensures audio captured before VADSpeechStart is included in transcription
	if ms.audioBuf.Len() > 0 {
		data := make([]byte, ms.audioBuf.Len())
		copy(data, ms.audioBuf.Bytes())
		ms.lastUserAudio = append(ms.lastUserAudio, data...)
		ms.audioBuf.Reset()
		ms.mu.Unlock()

		// Use blocking send to ensure pre-buffered audio is not discarded
		select {
		case sttChan <- data:
			// Successfully sent buffered audio to STT
		case <-ctx.Done():
			// Context cancelled during buffer send
			return
		}
	} else {
		ms.mu.Unlock()
	}
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	// DO NOT interrupt here. Wait for a valid transcript first!

	ms.mu.Lock()
	previousCancel := ms.pipelineCancel
	ctx, cancel := context.WithTimeout(ms.ctx, 15*time.Second)

	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttStartTime = time.Now()
	ms.lastUserAudio = make([]byte, len(audioData))
	copy(ms.lastUserAudio, audioData)
	ms.mu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	defer cancel()

	ms.mu.Lock()
	ms.sttRequestStartTime = time.Now()
	ms.mu.Unlock()
	fmt.Printf("\r\033[K[DEBUG] Calling Transcribe for %d bytes\n", len(audioData))
	result, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	ms.mu.Lock()
	if err == nil {
		fmt.Printf("\r\033[K[DEBUG] Transcribe returned: '%s' (prob=%.2f)\n", result.Text, result.NoSpeechProb)
		ms.sttEndTime = time.Now()
		ms.lastNoSpeechProb = result.NoSpeechProb
	} else {
		fmt.Printf("\r\033[K[DEBUG] Transcribe error: %v\n", err)
	}
	ms.mu.Unlock()

	if err != nil {
		if ctx.Err() == nil {
			fmt.Printf("\r\033[K[DEBUG] Transcribe error: %v\n", err)
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	audioDuration := time.Since(ms.userSpeechStartTime)
	if !ms.userSpeechEndTime.IsZero() {
		audioDuration = ms.userSpeechEndTime.Sub(ms.userSpeechStartTime)
	}

	ms.mu.Lock()
	speaking := ms.isSpeaking
	ms.mu.Unlock()

	if result.Text == "" || ms.isLikelyNoise(result, audioDuration, speaking) {
		if result.Text != "" {
			fmt.Printf("\r\033[K🔄 [NOISE] Rejected hallucination: '%s' (prob=%.2f, dur=%v)\n", result.Text, result.NoSpeechProb, audioDuration)
		}
		ms.emit(BotResumed, nil)
		return
	}

	transcript := result.Text

	// Before committing to interrupt, check if user is still speaking
	// If they resumed during transcription processing, discard and keep listening
	ms.mu.Lock()
	userStillSpeaking := ms.vad != nil && ms.vad.IsSpeaking()
	thinking := ms.isThinking
	ms.mu.Unlock()

	if userStillSpeaking {
		fmt.Printf("\r\033[K[DEBUG] User resumed speaking during transcription processing, discarding result and continuing to listen\n")
		return
	}

	if speaking {
		minWords := 1
		if ms.orch != nil {
			minWords = ms.orch.GetConfig().MinWordsToInterrupt
		}
		if minWords > 1 && countWords(transcript) < minWords {
			ms.mu.Lock()
			if rmsVAD, ok := ms.vad.(*RMSVAD); ok && rmsVAD.IsSpeaking() {
				ms.mu.Unlock()
				return
			}
			ms.mu.Unlock()
			return
		}
		ms.internalInterrupt()
	} else if thinking {
		ms.internalInterrupt()
	}

	// Echo guard: if the bot was speaking, the transcription is likely
	// echo from TTS playback. Skip to prevent the interrupt loop.
	if speaking && ms.isEchoTranscript(transcript) {
		fmt.Printf("\r\033[K🌀 [ECHO] Skipped echo transcript: '%s'\n", transcript)
		ms.emit(BotResumed, nil)
		return
	}

	ms.emit(TranscriptFinal, transcript)
	ms.mu.Lock()
	if ms.inPreemptiveTurn {
		ms.mu.Unlock()
		ms.session.UpdateLastUserMessage(transcript)
	} else {
		ms.inPreemptiveTurn = true
		ms.mu.Unlock()
		ms.session.AddMessage("user", transcript)
	}

	ms.runLLMAndTTS(ctx, transcript)
}

func (ms *ManagedStream) runLLMAndTTS(ctx context.Context, transcript string) {
	ms.mu.Lock()
	if ms.orch == nil || ms.session == nil {
		ms.mu.Unlock()
		return
	}

	// Debug: trace LLM calls
	fmt.Printf("\r\033[K🔍 [runLLMAndTTS] called with transcript: %q\n", transcript)

	if transcript != "" {
		ms.turnCount++
	}

	if ms.shouldSummarizeContext() {
		go ms.summarizeContext(ctx)
	}

	if ms.responseCancel != nil {
		ms.responseCancel()
	}
	if ms.ttsCancel != nil {
		ms.ttsCancel()
	}

	rCtx, rCancel := context.WithCancel(ctx)
	ms.responseCancel = rCancel
	ms.isThinking = true
	ms.payloadGen++
	gen := ms.payloadGen

	if transcript != "" {
		ms.toolRecursionDepth = 0
	}

	ms.mu.Unlock()

	defer rCancel()

	ms.emitWithGen(BotThinking, nil, gen)

	ms.mu.Lock()
	ms.llmStartTime = time.Now()
	ms.mu.Unlock()

	if sProvider, ok := ms.orch.llm.(StreamingLLMProvider); ok {
		ms.runStreamingLLMPipeline(rCtx, sProvider)
		return
	}

	// Fallback to batch logic
	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	ms.mu.Lock()
	if err == nil {
		ms.llmEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)
	ms.emit(BotResponse, response)

	ttsCtx, ttsCancel := context.WithCancel(rCtx)
	defer ttsCancel()
	ms.speakText(ttsCtx, response)
}

func (ms *ManagedStream) runStreamingLLMPipeline(ctx context.Context, provider StreamingLLMProvider) {
	useSentenceStreaming := ms.config.SentenceStreaming

	if useSentenceStreaming {
		ms.runStreamingLLMPipelineWithSentences(ctx, provider)
		return
	}

	ms.runStreamingLLMPipelineLegacy(ctx, provider)
}

func (ms *ManagedStream) runStreamingLLMPipelineWithSentences(ctx context.Context, provider StreamingLLMProvider) {
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()

	type pendingToolResult struct {
		tc     ToolCallEventData
		result string
	}
	var toolResults []pendingToolResult
	var toolCallCount int

	sentenceCh := make(chan string, 16)
	ttsSeqCtx, ttsSeqCancel := context.WithCancel(ctx)
	defer ttsSeqCancel()

	ttsDone := make(chan struct{})
	go ms.ttsSequencer(ttsSeqCtx, sentenceCh, ttsDone)

	hasSpoken := false

	_, err := provider.StreamComplete(ctx, messages, ms.session.GetTools(), func(chunk string) error {
		fullText.WriteString(chunk)

		ms.mu.Lock()
		if ms.llmEndTime.IsZero() {
			ms.llmEndTime = time.Now()
		}
		ms.mu.Unlock()

		if ms.config.ExpressiveMode {
			chunk = stripExpressiveTags(chunk)
		}

		if ms.sentenceBuffer != nil {
			if sentence := ms.sentenceBuffer.Feed(chunk); sentence != "" {
				hasSpoken = true
				select {
				case sentenceCh <- sentence:
				case <-ctx.Done():
				}
			}
		}

		return nil
	}, func(tc ToolCallEventData) error {
		toolCallCount++
		hasToolCalls = true
		ms.emit(ToolCall, tc)

		if toolCallCount == 1 && ms.sentenceBuffer != nil {
			ms.sentenceBuffer.Reset()
		}

		o := ms.orch
		o.mu.RLock()
		handler, ok := o.toolHandlers[tc.Name]
		o.mu.RUnlock()

		result := "Error: tool not found"
		if ok {
			var err error
			result, err = handler(tc.Arguments)
			if err != nil {
				result = fmt.Sprintf("Error: %v", err)
			}
		}

		toolResults = append(toolResults, pendingToolResult{tc: tc, result: result})
		return nil
	})

	if err != nil {
		close(sentenceCh)
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Streaming LLM error: %v", err))
		}
		return
	}

	remainingSentences := ms.sentenceBuffer.DrainRemaining()
	fmt.Printf("\r\033[K🔍 [DRAIN] %d remaining sentences\n", len(remainingSentences))
	for _, s := range remainingSentences {
		if s == "" {
			continue
		}
		fmt.Printf("\r\033[K🔍 [DRAIN-SEND] %q (len=%d)\n", s[:minInt(50, len(s))], len(s))
		hasSpoken = true
		select {
		case sentenceCh <- s:
		case <-ctx.Done():
		}
	}

	close(sentenceCh)

	<-ttsDone

	response := strings.TrimSpace(fullText.String())

	if hasSpoken && response != "" {
		fmt.Printf("\r\033[K🔍 [PATH-A] hasSpoken=true, response=%q\n", response[:min(30, len(response))])
		if !hasToolCalls {
			ms.session.AddMessage("assistant", response)
		}
		ms.emit(BotResponse, response)
	} else if response != "" {
		fmt.Printf("\r\033[K🔍 [PATH-B] hasSpoken=%v, response=%q - CALLING SPEAKTEXT!\n", hasSpoken, response[:min(30, len(response))])
		if !hasToolCalls {
			ms.session.AddMessage("assistant", response)
		}
		ms.emit(BotResponse, response)
		ms.speakText(ctx, response)
	} else {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
	}

	if hasToolCalls && len(toolResults) > 0 {
		var tcData []interface{}
		for _, tr := range toolResults {
			tcData = append(tcData, map[string]interface{}{
				"id":   tr.tc.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tr.tc.Name,
					"arguments": tr.tc.Arguments,
				},
			})
		}

		ms.session.AddMessageRaw(Message{
			Role:      "assistant",
			Content:   response,
			ToolCalls: tcData,
		})

		for _, tr := range toolResults {
			ms.session.AddMessageRaw(Message{
				Role:       "tool",
				Content:    tr.result,
				ToolCallID: tr.tc.CallID,
			})
		}

		ms.mu.Lock()
		ms.toolRecursionDepth++
		depth := ms.toolRecursionDepth
		ms.mu.Unlock()

		if depth > 3 {
			ms.mu.Lock()
			ms.isThinking = false
			ms.mu.Unlock()
			return
		}

		freshCtx, cancel := context.WithCancel(ms.ctx)
		go func() {
			defer cancel()
			ms.runLLMAndTTS(freshCtx, "")
			ms.mu.Lock()
			ms.toolRecursionDepth--
			ms.mu.Unlock()
		}()
	}
}

func (ms *ManagedStream) runStreamingLLMPipelineLegacy(ctx context.Context, provider StreamingLLMProvider) {
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()

	type pendingToolResult struct {
		tc     ToolCallEventData
		result string
	}
	var toolResults []pendingToolResult
	var toolCallCount int

	_, err := provider.StreamComplete(ctx, messages, ms.session.GetTools(), func(chunk string) error {
		fullText.WriteString(chunk)
		ms.mu.Lock()
		if ms.llmEndTime.IsZero() {
			ms.llmEndTime = time.Now()
		}
		ms.mu.Unlock()
		return nil
	}, func(tc ToolCallEventData) error {
		toolCallCount++
		hasToolCalls = true
		ms.emit(ToolCall, tc)

		o := ms.orch
		o.mu.RLock()
		handler, ok := o.toolHandlers[tc.Name]
		o.mu.RUnlock()

		result := "Error: tool not found"
		if ok {
			var err error
			result, err = handler(tc.Arguments)
			if err != nil {
				result = fmt.Sprintf("Error: %v", err)
			}
		}

		toolResults = append(toolResults, pendingToolResult{tc: tc, result: result})
		return nil
	})

	if err != nil {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Streaming LLM error: %v", err))
		}
		return
	}

	response := strings.TrimSpace(fullText.String())

	if response != "" {
		if !hasToolCalls {
			ms.session.AddMessage("assistant", response)
		}
		ms.emit(BotResponse, response)

		ttsCtx, ttsCancel := context.WithCancel(ctx)
		defer ttsCancel()
		ms.speakText(ttsCtx, response)
	} else {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
	}

	if hasToolCalls {
		var tcData []interface{}
		for _, tr := range toolResults {
			tcData = append(tcData, map[string]interface{}{
				"id":   tr.tc.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tr.tc.Name,
					"arguments": tr.tc.Arguments,
				},
			})
		}

		ms.session.AddMessageRaw(Message{
			Role:      "assistant",
			Content:   response,
			ToolCalls: tcData,
		})

		for _, tr := range toolResults {
			ms.session.AddMessageRaw(Message{
				Role:       "tool",
				Content:    tr.result,
				ToolCallID: tr.tc.CallID,
			})
		}

		ms.mu.Lock()
		ms.toolRecursionDepth++
		depth := ms.toolRecursionDepth
		ms.mu.Unlock()

		if depth > 3 {
			ms.mu.Lock()
			ms.isThinking = false
			ms.mu.Unlock()
			return
		}

		freshCtx, cancel := context.WithCancel(ms.ctx)
		go func() {
			defer cancel()
			ms.runLLMAndTTS(freshCtx, "")
			ms.mu.Lock()
			ms.toolRecursionDepth--
			ms.mu.Unlock()
		}()
	}
}

func (ms *ManagedStream) speakText(ctx context.Context, text string) {
	// Debug: trace batch TTS calls
	fmt.Printf("\r\033[K🔊 [speakText] text: %q\n", text)

	// Create a sub-context that we can cancel specifically if interrupted
	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true
	if ms.vad != nil {
		ms.vad.Reset()
	}
	ms.ttsCancel = sCancel
	ms.botSpeakStartTime = time.Now()
	ms.ttsStartTime = ms.botSpeakStartTime

	// Only reset the user audio buffer if we are NOT currently being interrupted
	// or if the user hasn't already started a new turn.
	if ms.vad == nil || !ms.vad.IsSpeaking() {
		fmt.Printf("\r\033[K[DEBUG] Resetting audio buffer at start of bot speech\n")
		ms.audioBuf.Reset()
		ms.lastUserAudio = nil
		ms.userSpeechStartTime = time.Time{}
		ms.inPreemptiveTurn = false
	} else {
		fmt.Printf("\r\033[K[DEBUG] NOT resetting audio buffer - user is already speaking!\n")
	}
	ms.mu.Unlock()

	ms.emit(BotSpeaking, nil)

	ms.mu.Lock()
	pRate := ms.playbackRate
	gen := ms.payloadGen
	ms.mu.Unlock()

	// Detect streaming TTS providers (Deepgram workaround).
	// For streaming providers we skip jitter buffer entirely — chunks arrive
	// smoothly from the remote API and the 200ms buffer is pure dead latency.
	isStreamingTTS := ms.orch.GetProviders()["tts"] == "deepgram"
	if isStreamingTTS {
		fmt.Printf("\r\033[K[DEBUG] Streaming TTS detected — bypassing jitter buffer\n")
	}

	// JITTER BUFFER for single-core ARM:
	// On Cobalt100, TTS chunks can arrive late due to ONNX scheduling jitter.
	// We buffer audio before starting playback to create a runway that absorbs
	// sporadic slowdowns. Configurable via env var; default 200ms for ARM,
	// but can be lowered to 50-100ms on multi-core systems for lower latency.
	jitterBufferMs := 200
	if env := os.Getenv("JITTER_BUFFER_MS"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v >= 0 {
			jitterBufferMs = v
		}
	}
	frameSize := int(float64(pRate)*0.06) * 2 // 60ms frames (was 20ms)
	if frameSize <= 0 {
		frameSize = 5292 // Fallback to 44.1k 60ms
	}
	jitterTargetBytes := int(float64(pRate)*float64(jitterBufferMs)/1000.0) * 2
	var jitterBuf []byte
	hasStartedPlayback := false

	err := ms.orch.SynthesizeStream(sCtx, text, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		ms.mu.Lock()
		ms.lastAudioSentAt = time.Now()
		ms.mu.Unlock()

		if isStreamingTTS {
			// Streaming provider: emit immediately in 60ms frames, no buffering
			for i := 0; i < len(chunk); i += frameSize {
				end := i + frameSize
				if end > len(chunk) {
					end = len(chunk)
				}
				c := make([]byte, end-i)
				copy(c, chunk[i:end])
				ms.emitWithGen(AudioChunk, c, gen)
			}
			return nil
		}

		if !hasStartedPlayback {
			jitterBuf = append(jitterBuf, chunk...)
			if len(jitterBuf) >= jitterTargetBytes {
				hasStartedPlayback = true
				// Emit buffered audio in 60ms frames
				for i := 0; i < len(jitterBuf); i += frameSize {
					end := i + frameSize
					if end > len(jitterBuf) {
						end = len(jitterBuf)
					}
					c := make([]byte, end-i)
					copy(c, jitterBuf[i:end])
					ms.emitWithGen(AudioChunk, c, gen)
				}
				jitterBuf = nil
			}
			return nil
		}

		// Playback already started: emit immediately in 60ms frames
		for i := 0; i < len(chunk); i += frameSize {
			end := i + frameSize
			if end > len(chunk) {
				end = len(chunk)
			}
			c := make([]byte, end-i)
			copy(c, chunk[i:end])
			ms.emitWithGen(AudioChunk, c, gen)
		}
		return nil
	})

	// Flush any remaining jitter buffer at end-of-stream
	if !hasStartedPlayback && len(jitterBuf) > 0 {
		for i := 0; i < len(jitterBuf); i += frameSize {
			end := i + frameSize
			if end > len(jitterBuf) {
				end = len(jitterBuf)
			}
			c := make([]byte, end-i)
			copy(c, jitterBuf[i:end])
			ms.emitWithGen(AudioChunk, c, gen)
		}
	}

	if err != nil && sCtx.Err() == nil {
		fmt.Printf("\r\033[K[DEBUG] TTS error: %v\n", err)
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	if ms.ttsCancel != nil {
		// Only clear it if it's still pointing to our local cancel
		// This is a bit tricky but simple enough for local logic
		ms.ttsCancel = nil
	}
	ms.mu.Unlock()
}

func (ms *ManagedStream) ttsSequencer(ctx context.Context, sentences <-chan string, done chan<- struct{}) {
	defer func() {
		if done != nil {
			close(done)
		}
	}()

	hasEmittedSpeaking := false
	var lastSentence string

	// Capture generation at start to detect stale calls after interruption
	startGen := ms.payloadGen

	for {
		select {
		case <-ctx.Done():
			return
		case sentence, ok := <-sentences:
			if !ok {
				return
			}

			// Check if this is a stale call from previous generation
			ms.mu.Lock()
			currentGen := ms.payloadGen
			ms.mu.Unlock()
			if currentGen != startGen {
				fmt.Printf("\r\033[K⚠️ [STALE-TTS] Generation mismatch: expected %d, got %d\n", startGen, currentGen)
				return
			}

			if sentence == "" {
				continue
			}

			// Guard: skip if same sentence was just synthesized (prevents duplicates after interruption)
			if lastSentence == sentence {
				fmt.Printf("\r\033[K⚠️ [DUPLICATE-GUARD] Skipping duplicate: %q\n", sentence)
				lastSentence = sentence
				continue
			}
			lastSentence = sentence

			if !hasEmittedSpeaking {
				ms.mu.Lock()
				ms.isThinking = false
				ms.isSpeaking = true
				if ms.vad != nil {
					ms.vad.Reset()
				}
				ms.botSpeakStartTime = time.Now()
				ms.ttsStartTime = ms.botSpeakStartTime

				if ms.vad == nil || !ms.vad.IsSpeaking() {
					ms.audioBuf.Reset()
					ms.lastUserAudio = nil
					ms.userSpeechStartTime = time.Time{}
					ms.inPreemptiveTurn = false
				}
				ms.mu.Unlock()

				ms.emit(BotSpeaking, nil)
				hasEmittedSpeaking = true
			}

			ms.synthesizeSentence(ctx, sentence)
		}
	}
}

func (ms *ManagedStream) synthesizeSentence(ctx context.Context, sentence string) {
	ms.mu.Lock()
	pRate := ms.playbackRate
	gen := ms.payloadGen
	isStreamingTTS := ms.orch != nil && ms.orch.GetProviders()["tts"] == "deepgram"
	ms.mu.Unlock()

	// Debug: trace TTS calls
	fmt.Printf("\r\033[K🔊 [synthesizeSentence] sentence: %q\n", sentence)

	jitterBufferMs := ms.adaptiveJitterMs()
	frameSize := int(float64(pRate)*0.06) * 2
	if frameSize <= 0 {
		frameSize = 5292
	}
	jitterTargetBytes := int(float64(pRate)*float64(jitterBufferMs)/1000.0) * 2

	var jitterBuf []byte
	hasStartedPlayback := false

	err := ms.orch.SynthesizeStream(ctx, sentence, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		// Check if this generation is still active (not cancelled/interrupted)
		ms.mu.Lock()
		currentGen := ms.payloadGen
		ms.mu.Unlock()
		if currentGen != gen {
			return nil // Skip audio from cancelled generation
		}

		ms.mu.Lock()
		ms.lastAudioSentAt = time.Now()
		ms.mu.Unlock()

		if isStreamingTTS {
			for i := 0; i < len(chunk); i += frameSize {
				end := i + frameSize
				if end > len(chunk) {
					end = len(chunk)
				}
				c := make([]byte, end-i)
				copy(c, chunk[i:end])
				ms.emitWithGen(AudioChunk, c, gen)
			}
			return nil
		}

		if !hasStartedPlayback {
			jitterBuf = append(jitterBuf, chunk...)
			if len(jitterBuf) >= jitterTargetBytes {
				hasStartedPlayback = true
				for i := 0; i < len(jitterBuf); i += frameSize {
					end := i + frameSize
					if end > len(jitterBuf) {
						end = len(jitterBuf)
					}
					c := make([]byte, end-i)
					copy(c, jitterBuf[i:end])
					ms.emitWithGen(AudioChunk, c, gen)
				}
				jitterBuf = nil
			}
			return nil
		}

		for i := 0; i < len(chunk); i += frameSize {
			end := i + frameSize
			if end > len(chunk) {
				end = len(chunk)
			}
			c := make([]byte, end-i)
			copy(c, chunk[i:end])
			ms.emitWithGen(AudioChunk, c, gen)
		}
		return nil
	})

	if !hasStartedPlayback && len(jitterBuf) > 0 {
		for i := 0; i < len(jitterBuf); i += frameSize {
			end := i + frameSize
			if end > len(jitterBuf) {
				end = len(jitterBuf)
			}
			c := make([]byte, end-i)
			copy(c, jitterBuf[i:end])
			ms.emitWithGen(AudioChunk, c, gen)
		}
	}

	if err != nil && ctx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS sentence error: %v", err))
	}
}

func (ms *ManagedStream) adaptiveJitterMs() int {
	base := 200
	if env := os.Getenv("JITTER_BUFFER_MS"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v >= 0 {
			base = v
		}
	}

	ms.mu.Lock()
	isStreaming := ms.orch != nil && ms.orch.GetProviders()["tts"] == "deepgram"
	ms.mu.Unlock()

	if isStreaming {
		return 0
	}

	if ms.config.SentenceStreaming {
		return minInt(base, 100)
	}

	return base
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var expressiveTagPatterns = []struct {
	open  string
	close string
}{
	{open: "[laughs]", close: "[/laughs]"},
	{open: "[whispers]", close: "[/whispers]"},
	{open: "[sighs]", close: "[/sighs]"},
	{open: "[excited]", close: "[/excited]"},
	{open: "[slow]", close: "[/slow]"},
	{open: "[fast]", close: "[/fast]"},
	{open: "[sad]", close: "[/sad]"},
	{open: "[angry]", close: "[/angry]"},
}

func stripExpressiveTags(text string) string {
	result := text
	for _, p := range expressiveTagPatterns {
		result = strings.ReplaceAll(result, p.open, "")
		result = strings.ReplaceAll(result, p.close, "")
	}
	result = strings.ReplaceAll(result, "[", "")
	result = strings.ReplaceAll(result, "]", "")
	return result
}

// isEchoTranscript checks if the transcript likely comes from the bot's own
// TTS playback being picked up by the microphone. It compares word overlap
// with the last assistant response.
func (ms *ManagedStream) isEchoTranscript(transcript string) bool {
	if ms.session == nil {
		return false
	}
	ctx := ms.session.GetContextCopy()
	if len(ctx) == 0 {
		return false
	}
	// Check the last 2 messages for assistant role
	var lastMsg Message
	found := false
	for i := len(ctx) - 1; i >= 0; i-- {
		if ctx[i].Role == "assistant" {
			lastMsg = ctx[i]
			found = true
			break
		}
	}
	if !found {
		return false
	}
	overlap := specSimilarity(transcript, lastMsg.Content)
	// Lower threshold to catch more echo - even partial word matches matter
	return overlap > 0.15
}

func (ms *ManagedStream) shouldSummarizeContext() bool {
	threshold := ms.config.ContextSummarizationThreshold
	if threshold <= 0 {
		return false
	}
	return ms.turnCount > 0 && ms.turnCount%threshold == 0
}

func (ms *ManagedStream) summarizeContext(ctx context.Context) {
	if ms.orch == nil || ms.orch.llm == nil {
		return
	}

	ms.mu.Lock()
	messages := ms.session.GetContextCopy()
	ms.mu.Unlock()

	if len(messages) < 6 {
		return
	}

	var textParts []string
	for _, m := range messages[:len(messages)-4] {
		if m.Role == "user" || m.Role == "assistant" {
			textParts = append(textParts, m.Role+": "+m.Content)
		}
	}

	if len(textParts) < 3 {
		return
	}

	prompt := "Summarize the key points from this conversation history in 2-3 sentences." +
		" Focus on facts established, user preferences, and any decisions made.\n\n" + strings.Join(textParts, "\n")

	summary, err := ms.orch.llm.Complete(ctx, []Message{
		{Role: "system", Content: "You are a conversation summarizer. Output ONLY a brief summary."},
		{Role: "user", Content: prompt},
	}, nil)

	if err != nil || summary == "" {
		return
	}

	ms.mu.Lock()
	keep := ms.session.Context[len(ms.session.Context)-4:]
	ms.session.Context = append([]Message{
		{Role: "system", Content: fmt.Sprintf("[Conversation summary of previous turns]: %s", summary)},
	}, keep...)
	ms.turnCount = 0
	ms.mu.Unlock()
}

func (ms *ManagedStream) NotifyAudioPlayed() {
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now()
	ms.mu.Unlock()
}

func (ms *ManagedStream) RecordPlayedOutput(chunk []byte) {
	if ms.echoSuppressor == nil || len(chunk) == 0 {
		return
	}
	ms.echoSuppressor.RecordPlayedAudio(chunk)
}

func (ms *ManagedStream) GetLatency() int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.userSpeechEndTime.IsZero() || ms.botSpeakStartTime.IsZero() {
		return 0
	}

	if ms.botSpeakStartTime.Before(ms.userSpeechEndTime) {
		return 0
	}

	latency := ms.botSpeakStartTime.Sub(ms.userSpeechEndTime)
	return latency.Milliseconds()
}

type LatencyBreakdown struct {
	UserToSTT          int64
	UserToSTTStart     int64
	STT                int64
	STT_Internal       int64
	UserToLLM          int64
	LLM                int64
	UserToTTSFirstByte int64
	LLMToTTSFirstByte  int64
	TTSTotal           int64
	BotStartLatency    int64
	UserToPlay         int64
	NoSpeechProb       float64
}

func (ms *ManagedStream) GetEndToEndLatency() int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.userSpeechEndTime.IsZero() || ms.lastAudioSentAt.IsZero() {
		return 0
	}

	if ms.lastAudioSentAt.Before(ms.userSpeechEndTime) {
		return 0
	}

	latency := ms.lastAudioSentAt.Sub(ms.userSpeechEndTime)
	return latency.Milliseconds()
}

func (ms *ManagedStream) GetLatencyBreakdown() LatencyBreakdown {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	var bd LatencyBreakdown
	if ms.userSpeechEndTime.IsZero() {
		return bd
	}

	if !ms.sttEndTime.IsZero() {
		bd.UserToSTT = ms.sttEndTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.sttRequestStartTime.IsZero() {
		bd.UserToSTTStart = ms.sttRequestStartTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.sttStartTime.IsZero() && !ms.sttEndTime.IsZero() {
		bd.STT = ms.sttEndTime.Sub(ms.sttStartTime).Milliseconds()
	}
	if !ms.sttRequestStartTime.IsZero() && !ms.sttEndTime.IsZero() {
		bd.STT_Internal = ms.sttEndTime.Sub(ms.sttRequestStartTime).Milliseconds()
	}

	if !ms.llmEndTime.IsZero() {
		bd.UserToLLM = ms.llmEndTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.llmStartTime.IsZero() && !ms.llmEndTime.IsZero() {
		bd.LLM = ms.llmEndTime.Sub(ms.llmStartTime).Milliseconds()
	}

	if !ms.ttsFirstChunkTime.IsZero() {
		bd.UserToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.llmEndTime.IsZero() && !ms.ttsFirstChunkTime.IsZero() {
		bd.LLMToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ms.llmEndTime).Milliseconds()
	}

	if !ms.ttsStartTime.IsZero() && !ms.ttsEndTime.IsZero() {
		bd.TTSTotal = ms.ttsEndTime.Sub(ms.ttsStartTime).Milliseconds()
	}

	if !ms.botSpeakStartTime.IsZero() {
		bd.BotStartLatency = ms.botSpeakStartTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.lastAudioSentAt.IsZero() {
		bd.UserToPlay = ms.lastAudioSentAt.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	bd.NoSpeechProb = ms.lastNoSpeechProb

	return bd
}

func (ms *ManagedStream) ExportLastUserAudio() (raw []byte, processed []byte) {
	ms.mu.Lock()
	if len(ms.lastUserAudio) == 0 {
		ms.mu.Unlock()
		return nil, nil
	}
	rawCopy := make([]byte, len(ms.lastUserAudio))
	copy(rawCopy, ms.lastUserAudio)
	ms.mu.Unlock()

	if ms.echoSuppressor != nil {
		processed = ms.echoSuppressor.PostProcess(rawCopy)
	} else {
		processed = rawCopy
	}
	return rawCopy, processed
}

func (ms *ManagedStream) Events() <-chan OrchestratorEvent {
	return ms.events
}

func (ms *ManagedStream) Close() {
	ms.closeOnce.Do(func() {
		ms.interrupt()

		ms.mu.Lock()
		ms.isClosed = true
		ms.audioBuf.Reset()
		ms.mu.Unlock()

		ms.echoSuppressor.ClearEchoBuffer()

		ms.cancel()

		time.Sleep(10 * time.Millisecond)

		ms.mu.Lock()
		close(ms.events)
		ms.mu.Unlock()
	})
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
	if eventType != AudioChunk {
		ms.updateActivity()
	}
	ms.mu.Lock()
	gen := ms.payloadGen
	ms.mu.Unlock()
	ms.emitWithGen(eventType, data, gen)
}

func (ms *ManagedStream) emitWithGen(eventType EventType, data interface{}, gen int) {
	select {
	case <-ms.ctx.Done():
		return
	default:
	}

	ms.mu.Lock()
	if ms.isClosed {
		ms.mu.Unlock()
		return
	}

	if eventType == AudioChunk {
		speaking := ms.isSpeaking
		userInterrupting := ms.userInterrupting
		if !speaking || userInterrupting {
			ms.mu.Unlock()
			return
		}
	}

	sessionID := ms.session.ID
	ms.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
		}
	}()

	event := OrchestratorEvent{
		Type:       eventType,
		SessionID:  sessionID,
		Data:       data,
		Generation: gen,
	}

	if eventType == AudioChunk {
		select {
		case ms.events <- event:
		case <-ms.ctx.Done():
		default:
			// Only drop AudioChunks if full, but block for other events
		}
	} else {
		select {
		case ms.events <- event:
		case <-ms.ctx.Done():
		}
	}
}

func (ms *ManagedStream) interrupt() {
	ms.internalInterrupt()
}

func (ms *ManagedStream) internalInterrupt() {
	ms.mu.Lock()

	isStillPlaying := time.Since(ms.lastAudioSentAt) < time.Second

	if ms.responseCancel == nil && ms.ttsCancel == nil && !ms.isSpeaking && !ms.isThinking && !ms.userInterrupting && !isStillPlaying {
		ms.mu.Unlock()
		return
	}

	responseCancel := ms.responseCancel
	ttsCancel := ms.ttsCancel

	ms.lastActivityAt = time.Now()

	ms.responseCancel = nil
	ms.ttsCancel = nil

	if ms.userSpeechEndTime.IsZero() {
		ms.userSpeechEndTime = time.Now()
	}
	ms.sttEndTime = ms.userSpeechEndTime

	ms.isSpeaking = false
	ms.isThinking = false
	ms.userInterrupting = false
	gen := ms.payloadGen
	ms.mu.Unlock()

	if ms.speculator != nil {
		ms.speculator.Reset()
	}
	if ms.sentenceBuffer != nil {
		ms.sentenceBuffer.Reset()
	}

	ms.echoSuppressor.ClearEchoBuffer()

	if responseCancel != nil {
		responseCancel()
	}
	if ttsCancel != nil {
		ttsCancel()
	}

	if ms.orch != nil && ms.orch.tts != nil {
		if err := ms.orch.tts.Abort(); err != nil {
			ms.orch.logger.Warn("tts abort failed", "sessionID", ms.session.ID, "error", err)
		}
	}

	ms.emitWithGen(Interrupted, nil, gen)
	ms.drainAudioChunks()
}

func (ms *ManagedStream) drainAudioChunks() {
	deadline := time.Now().Add(100 * time.Millisecond)
	var controlEvents []OrchestratorEvent

	for {
		select {
		case ev := <-ms.events:
			if ev.Type != AudioChunk {
				controlEvents = append(controlEvents, ev)
			}
		default:
			goto DrainDone
		}

		if time.Now().After(deadline) {
			goto DrainDone
		}
	}

DrainDone:
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.isClosed {
		return
	}
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
		}
	}
}

func (ms *ManagedStream) updateActivity() {
	ms.mu.Lock()
	ms.lastActivityAt = time.Now()
	ms.mu.Unlock()
}

func (ms *ManagedStream) monitorInactivity() {
	ms.mu.Lock()
	timeout := 10 * time.Second
	if ms.orch != nil {
		timeout = ms.orch.config.SilenceTimeout
	}
	ms.mu.Unlock()

	if timeout <= 0 {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ms.ctx.Done():
			return
		case <-ticker.C:
			ms.mu.Lock()
			thinking := ms.isThinking
			speaking := ms.isSpeaking
			userSpeaking := false
			if ms.vad != nil {
				userSpeaking = ms.vad.IsSpeaking()
			}
			lastActivity := ms.lastActivityAt
			closed := ms.isClosed
			ms.mu.Unlock()

			if closed {
				return
			}

			// If nobody is doing anything for the timeout period, trigger a re-prompt.
			if !thinking && !speaking && !userSpeaking {
				if time.Since(lastActivity) > timeout {
					ms.updateActivity() // Prevent spamming
					fmt.Printf("\r\033[K[DEBUG] Inactivity guard fired (%v silence). Reprompting...\n", timeout)

					// We inject a hidden user message [SILENCE] to trigger a natural follow-up
					go ms.runSilenceCheck()
				}
			}
		}
	}
}

func (ms *ManagedStream) runSilenceCheck() {
	ms.mu.Lock()
	if ms.orch == nil || ms.orch.llm == nil {
		ms.mu.Unlock()
		return
	}
	if ms.isThinking || ms.isSpeaking || (ms.vad != nil && ms.vad.IsSpeaking()) {
		ms.mu.Unlock()
		return
	}
	ctx := ms.ctx
	ms.mu.Unlock()

	// Ask the LLM to handle the silence naturally
	ms.runLLMAndTTS(ctx, "[USER_SILENCE_TIMEOUT]")
}
