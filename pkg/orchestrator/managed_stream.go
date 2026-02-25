package orchestrator

import (
	"bytes"
	"context"
	"fmt"
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

	pipelineCtx       context.Context
	pipelineCancel    context.CancelFunc
	sttChan           chan<- []byte
	sttGeneration     int
	isSpeaking        bool
	isThinking        bool
	lastInterruptedAt time.Time
	lastAudioSentAt   time.Time
	userSpeechEndTime time.Time
	botSpeakStartTime time.Time

	lastUserAudio []byte

	sttStartTime      time.Time
	sttEndTime        time.Time
	llmStartTime      time.Time
	llmEndTime        time.Time
	ttsStartTime      time.Time
	ttsFirstChunkTime time.Time
	ttsEndTime        time.Time

	responseCancel     context.CancelFunc
	ttsCancel          context.CancelFunc
	userInterrupting   bool
	echoSuppressor     *EchoSuppressor
	lastAudioEmittedAt time.Time
	closeOnce          sync.Once

	payloadGen int
	writeChan  chan []byte
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
		writeChan:      make(chan []byte, 1024),
	}

	go ms.processBackgroundAudio()

	if o != nil && o.config.FirstSpeaker == FirstSpeakerBot {
		go func() {
			time.Sleep(500 * time.Millisecond) // Give audio some time to stabilize
			ms.runLLMAndTTS(ms.ctx, "Hello!")  // Trigger initial greeting
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
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		return rmsVAD.IsSpeaking()
	}
	return false
}

func (ms *ManagedStream) SetEchoSampleRates(playbackRate, inputRate int) {
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

const speechEndHold = 150 * time.Millisecond

func (ms *ManagedStream) Write(chunk []byte) error {
	select {
	case ms.writeChan <- chunk:
		return nil
	default:
		// Channel full, drop audio or log warning
		return nil
	}
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

	vadTrailWindow := 1500 * time.Millisecond
	vadThreshold := 0.0
	if ms.orch != nil {
		vadTrailWindow = ms.orch.GetConfig().BargeInVADTrailWindow
		vadThreshold = ms.orch.GetConfig().BargeInVADThreshold
	}

	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		originalThreshold := rmsVAD.Threshold()
		originalMinConfirmed := rmsVAD.MinConfirmed()

		ms.mu.Lock()
		speaking := ms.isSpeaking
		isThinking := ms.isThinking
		ms.mu.Unlock()

		lastEmitted := ms.lastAudioEmittedAt
		inTrail := time.Since(lastEmitted) < vadTrailWindow
		if speaking || isThinking || inTrail {
			// When the bot is active, we are MORE cautious to prevent self-interruption.
			// We raise the threshold to at least 0.015, unless the base threshold is already higher.
			target := 0.015
			if vadThreshold > target {
				target = vadThreshold
			}
			rmsVAD.SetThreshold(target)
			rmsVAD.SetMinConfirmed(2)
			rmsVAD.SetAdaptiveMode(false)
		} else {
			// When idle, we use the base sensitivity (0.005).
			rmsVAD.SetThreshold(vadThreshold)
			rmsVAD.SetMinConfirmed(2)
			rmsVAD.SetAdaptiveMode(true)
		}

		defer func() {
			rmsVAD.SetThreshold(originalThreshold)
			rmsVAD.SetMinConfirmed(originalMinConfirmed)
			rmsVAD.SetAdaptiveMode(true)
		}()
	}

	// Restore passive echo detection solely for debugging/logging purposes.
	// It does NOT gate VAD events anymore.
	isEcho := false
	if ms.echoSuppressor != nil {
		ms.mu.Lock()
		lead := ms.audioBuf.Bytes()
		ms.mu.Unlock()

		leadBytes := 8820
		if len(lead) > leadBytes {
			lead = lead[len(lead)-leadBytes:]
		}
		checkBuf := make([]byte, 0, len(lead)+len(chunk))
		checkBuf = append(checkBuf, lead...)
		checkBuf = append(checkBuf, chunk...)

		if ms.echoSuppressor.IsEchoFast(checkBuf) {
			isEcho = true
		}
	}

	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {

		switch event.Type {
		case VADSpeechStart:
			// Pipecat/Vocode pattern: we trigger an immediate interrupt upon VAD detection
			// unless it's a confirmed echo, to achieve high responsiveness.
			if !isEcho {
				ms.internalInterrupt()
			}
			ms.emit(UserSpeaking, nil)

			ms.mu.Lock()
			ms.sttGeneration++
			pipelineCancel := ms.pipelineCancel
			ms.pipelineCancel = nil
			ms.sttChan = nil

			ms.sttStartTime = time.Now()
			ms.sttEndTime = time.Time{}
			ms.llmStartTime = time.Time{}
			ms.llmEndTime = time.Time{}
			ms.ttsStartTime = time.Time{}
			ms.ttsFirstChunkTime = time.Time{}
			ms.ttsEndTime = time.Time{}
			ms.lastUserAudio = nil
			ms.mu.Unlock()

			if pipelineCancel != nil {
				pipelineCancel()
			}

			if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
				ms.startStreamingSTT(sProvider)
			}
		case VADSpeechEnd:
			ms.mu.Lock()
			ms.userSpeechEndTime = time.Now()
			ms.mu.Unlock()
			ms.emit(UserStopped, nil)

			ms.mu.Lock()
			sttChan := ms.sttChan
			if sttChan != nil {
				ms.sttChan = nil
				ms.mu.Unlock()
			} else {
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				ms.mu.Unlock()

				go func(buf []byte) {
					t := time.NewTimer(speechEndHold)
					defer t.Stop()

					select {
					case <-t.C:
						if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
							if rmsVAD.IsSpeaking() {
								ms.mu.Lock()
								ms.audioBuf.Write(buf)
								ms.mu.Unlock()
								return
							}
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

	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	ms.mu.Lock()
	ms.audioBuf.Write(chunk)
	if !isUserSpeaking && ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-132300:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}
	ms.mu.Unlock()

	ms.mu.Lock()
	sttChan := ms.sttChan
	if sttChan != nil {
		ms.lastUserAudio = append(ms.lastUserAudio, chunk...)
	}
	ms.mu.Unlock()

	if sttChan != nil {
		select {
		case sttChan <- chunk:
		default:
		}
	}

	return nil
}

func isLikelyNoise(transcript string, audioDuration time.Duration) bool {
	t := strings.TrimSpace(transcript)
	if t == "" {
		return true
	}

	if audioDuration < 100*time.Millisecond {
		return true
	}

	wordCount := len(strings.Fields(t))

	if wordCount == 1 && audioDuration > 5000*time.Millisecond {
		return true
	}

	if wordCount <= 2 && audioDuration > 8000*time.Millisecond {
		return true
	}

	return false
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
		ms.mu.Unlock()

		if isStale {
			return nil
		}

		ms.mu.Lock()
		minWords := 1
		if ms.orch != nil {
			minWords = ms.orch.GetConfig().MinWordsToInterrupt
		}
		duration := time.Since(ms.sttStartTime)
		ms.mu.Unlock()

		if speaking || thinking {
			wc := countWords(transcript)
			if minWords > 1 {
				if wc < minWords {
					if !isFinal {
						ms.emit(TranscriptPartial, transcript)
					}
					return nil
				}
				noise := isLikelyNoise(transcript, duration)
				if !noise {
					ms.internalInterrupt()
				}
			} else {
				noise := isLikelyNoise(transcript, duration)
				if strings.TrimSpace(transcript) != "" && !noise {
					ms.internalInterrupt()
				}
			}
		}

		if isFinal {
			ms.mu.Lock()
			ms.sttEndTime = time.Now()
			duration := time.Since(ms.sttStartTime)
			ms.mu.Unlock()

			if isLikelyNoise(transcript, duration) {
				return nil
			}

			ms.emit(TranscriptFinal, transcript)
			ms.session.AddMessage("user", transcript)
		} else {
			ms.emit(TranscriptPartial, transcript)
		}
		return nil
	})

	if err != nil {
		ms.emit(ErrorEvent, fmt.Sprintf("failed to start streaming STT: %v", err))
		cancel()
		return
	}

	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttChan = sttChan
	ms.sttStartTime = time.Now()

	if ms.audioBuf.Len() > 0 {
		data := make([]byte, ms.audioBuf.Len())
		copy(data, ms.audioBuf.Bytes())
		ms.lastUserAudio = make([]byte, len(data))
		copy(ms.lastUserAudio, data)
		ms.audioBuf.Reset()
		select {
		case sttChan <- data:
		default:
		}
	}
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	// DO NOT interrupt here. Wait for a valid transcript first!

	ms.mu.Lock()
	ctx, cancel := context.WithCancel(ms.ctx)
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttStartTime = time.Now()
	ms.lastUserAudio = make([]byte, len(audioData))
	copy(ms.lastUserAudio, audioData)
	ms.mu.Unlock()
	defer cancel()

	ms.emit(BotThinking, nil)

	transcript, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	ms.mu.Lock()
	if err == nil {
		ms.sttEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	audioDuration := time.Duration(len(audioData)/2) * time.Second / 44100

	if transcript == "" || isLikelyNoise(transcript, audioDuration) {
		return
	}

	ms.mu.Lock()
	speaking := ms.isSpeaking
	thinking := ms.isThinking
	ms.mu.Unlock()

	if speaking {
		minWords := 1
		if ms.orch != nil {
			minWords = ms.orch.GetConfig().MinWordsToInterrupt
		}
		if minWords > 1 && countWords(transcript) < minWords {
			return
		}
		ms.internalInterrupt()
	} else if thinking {
		ms.internalInterrupt()
	} else {
		ms.internalInterrupt()
	}

	ms.emit(TranscriptFinal, transcript)
	ms.session.AddMessage("user", transcript)

	ms.runLLMAndTTS(ctx, transcript)
}

func (ms *ManagedStream) runLLMAndTTS(ctx context.Context, transcript string) {
	ms.mu.Lock()

	if ms.responseCancel != nil {
		ms.responseCancel()
	}
	if ms.ttsCancel != nil {
		ms.ttsCancel()
	}

	rCtx, rCancel := context.WithCancel(ctx)
	ms.responseCancel = rCancel
	ms.isThinking = true
	ms.mu.Unlock()

	defer rCancel()

	ms.emit(BotThinking, nil)

	ms.mu.Lock()
	ms.llmStartTime = time.Now()
	ms.mu.Unlock()

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	ms.mu.Lock()
	if err == nil {
		ms.llmEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil {
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)
	ms.emit(BotResponse, response)

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true

	if ms.vad != nil {
		ms.vad.Reset()
	}

	ttsCtx, ttsCancel := context.WithCancel(rCtx)
	ms.ttsCancel = ttsCancel
	ms.mu.Unlock()

	defer ttsCancel()

	ms.mu.Lock()
	ms.botSpeakStartTime = time.Now()
	ms.ttsStartTime = ms.botSpeakStartTime
	ms.mu.Unlock()
	ms.emit(BotSpeaking, nil)

	err = ms.orch.SynthesizeStream(ttsCtx, response, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		select {
		case <-ttsCtx.Done():
			return ttsCtx.Err()
		default:
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.lastAudioEmittedAt = ms.lastAudioSentAt
			if ms.ttsFirstChunkTime.IsZero() {
				ms.ttsFirstChunkTime = time.Now()
			}
			gen := ms.payloadGen
			ms.mu.Unlock()

			// Slice large chunks into ~20ms frames to prevent playback jitter/underflows
			frameSize := 1764 // 44100Hz * 0.02s * 2 bytes
			for i := 0; i < len(chunk); i += frameSize {
				end := i + frameSize
				if end > len(chunk) {
					end = len(chunk)
				}
				c := chunk[i:end]

				ms.emitWithGen(AudioChunk, c, gen)
			}
			return nil
		}
	})

	ms.mu.Lock()
	if !ms.ttsStartTime.IsZero() {
		ms.ttsEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil && ttsCtx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	ms.ttsCancel = nil
	ms.mu.Unlock()
}

func (ms *ManagedStream) NotifyAudioPlayed() {
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now()
	ms.lastAudioEmittedAt = ms.lastAudioSentAt
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
	STT                int64
	UserToLLM          int64
	LLM                int64
	UserToTTSFirstByte int64
	LLMToTTSFirstByte  int64
	TTSTotal           int64
	BotStartLatency    int64
	UserToPlay         int64
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
	if !ms.sttStartTime.IsZero() && !ms.sttEndTime.IsZero() {
		bd.STT = ms.sttEndTime.Sub(ms.sttStartTime).Milliseconds()
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
		ms.audioBuf.Reset()
		ms.mu.Unlock()

		ms.echoSuppressor.ClearEchoBuffer()

		ms.cancel()

		time.Sleep(10 * time.Millisecond)

		close(ms.events)
	})
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
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

	if eventType == AudioChunk {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		userInterrupting := ms.userInterrupting
		ms.mu.Unlock()
		if !speaking || userInterrupting {
			return
		}
	}

	event := OrchestratorEvent{
		Type:       eventType,
		SessionID:  ms.session.ID,
		Data:       data,
		Generation: gen,
	}

	defer func() {
		if r := recover(); r != nil {
		}
	}()

	select {
	case ms.events <- event:
	case <-ms.ctx.Done():
	default:
	}
}

func (ms *ManagedStream) interrupt() {
	ms.internalInterrupt()
}

func (ms *ManagedStream) internalInterrupt() {
	ms.mu.Lock()

	// Check if there's anything to interrupt (TTS or LLM request)
	// We allow a 1-second window after isSpeaking=false to account for audio in the playback buffer.
	isStillPlaying := time.Since(ms.lastAudioSentAt) < time.Second

	if ms.responseCancel == nil && ms.ttsCancel == nil && !ms.isSpeaking && !ms.isThinking && !ms.userInterrupting && !isStillPlaying {
		ms.mu.Unlock()
		return
	}

	responseCancel := ms.responseCancel
	ttsCancel := ms.ttsCancel

	ms.responseCancel = nil
	ms.ttsCancel = nil

	ms.isSpeaking = false
	ms.isThinking = false
	ms.userInterrupting = false
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()

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

	ms.lastInterruptedAt = time.Now()
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
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
		}
	}
}
