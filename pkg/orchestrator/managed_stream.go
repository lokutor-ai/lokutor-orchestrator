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

	lastUserAudio []byte

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

	payloadGen       int
	writeChan        chan []byte
	isClosed         bool
	lastNoSpeechProb float64
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
	}

	go ms.processBackgroundAudio()

	if o != nil && o.config.FirstSpeaker == FirstSpeakerBot {
		go func() {
			time.Sleep(500 * time.Millisecond) // Give audio some time to stabilize
			// Add greeting to context first so LLM knows what it's saying
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
	// We MUST copy the chunk here because the caller (main.go) will recycle the
	// underlying buffer into the sync.Pool as soon as this function returns.
	// Without this copy, doWrite() would be processing memory that is being
	// simultaneously overwritten by the microphone callback.
	buf := make([]byte, len(chunk))
	copy(buf, chunk)

	ms.writeChan <- buf
	return nil
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

	// Apply echo suppression BEFORE VAD to prevent the bot from interrupting itself.
	// We use the "Fast" version to minimize latency impact on the real-time audio loop.
	vadChunk := chunk
	if ms.echoSuppressor != nil {
		vadChunk = ms.echoSuppressor.RemoveEchoRealtime(chunk)
	}

	event, err := ms.vad.Process(vadChunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
		switch event.Type {
		case VADSpeechPotential:
			// Instant mute signal disabled to wait for firmer confirmation
			// ms.emit(UserSpeaking, nil)

		case VADSpeechStart:
			ms.mu.Lock()
			ms.userSpeechStartTime = time.Now()
			ms.mu.Unlock()

			// We now emit UserSpeaking on a confirmed start to prevent glitchy pausing
			ms.emit(UserSpeaking, nil)

			ms.mu.Lock()
			ms.sttGeneration++
			pipelineCancel := ms.pipelineCancel
			sttChan := ms.sttChan
			ms.pipelineCancel = nil
			ms.sttChan = nil

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
			ms.lastUserAudio = nil
			ms.mu.Unlock()

			if pipelineCancel != nil {
				pipelineCancel()
			}
			if sttChan != nil {
				close(sttChan)
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
				close(sttChan)
			} else {
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				ms.mu.Unlock()

				go func(buf []byte) {
					ms.mu.Lock()
					duration := ms.userSpeechEndTime.Sub(ms.userSpeechStartTime)
					ms.mu.Unlock()

					// Fast-path: if the sound was very short, don't wait for another second
					// to see if the user continues. It's likely just noise.
					if duration < 400*time.Millisecond {
						ms.runBatchPipeline(buf)
						return
					}

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

	cleanChunk := chunk
	// Protect against byte-tearing on S16 PCM chunks
	if len(cleanChunk)%2 != 0 {
		cleanChunk = cleanChunk[:len(cleanChunk)-1]
	}

	ms.mu.Lock()
	ms.audioBuf.Write(cleanChunk)
	// Keep maximum 4 seconds of pristine audio (176400 bytes). Must slice on an EVEN byte boundary (2-byte samples).
	if !isUserSpeaking && ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		// Safe lead-in size (3 seconds = 132300 bytes). This is evenly divisible by 2.
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
		select {
		case sttChan <- toSend:
		default:
		}
	}

	return nil
}

func (ms *ManagedStream) isLikelyNoise(result TranscriptionResult, audioDuration time.Duration) bool {
	// If the STT engine is >= 70% sure this is not speech, trust it.
	if result.NoSpeechProb > 0.7 {
		return true
	}

	clean := strings.TrimSpace(result.Text)
	if clean == "" {
		return true
	}

	// Very short audio with minimal transcription is usually a hallucination from a click/breath
	if audioDuration < 800*time.Millisecond && len(clean) <= 15 {
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
				noise := ms.isLikelyNoise(TranscriptionResult{Text: transcript}, duration)
				if !noise {
					ms.internalInterrupt()
				}
			} else {
				noise := ms.isLikelyNoise(TranscriptionResult{Text: transcript}, duration)
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

			// Warning: Streaming transcribers may not provide NoSpeechProb, so we rely on heuristics
			if ms.isLikelyNoise(TranscriptionResult{Text: transcript}, duration) {
				fmt.Printf("\r\033[K🔄 [NOISE] Rejected hallucination: '%s' (dur=%v)\n", transcript, duration)
				ms.emit(BotResumed, nil)
				return nil
			}

			ms.emit(TranscriptFinal, transcript)
			ms.session.AddMessage("user", transcript)

			go ms.runLLMAndTTS(ctx, transcript)
		} else {
			ms.emit(TranscriptPartial, transcript)
		}
		return nil
	})

	if err != nil {
		// Just log or emit a warning, do not cancel the whole pipeline
		// because the orchestrator will gracefully fall back to batch Transcribe.
		fmt.Printf("Warning: could not start streaming STT (falling back to batch): %v\n", err)
	} else {
		ms.mu.Lock()
		ms.sttChan = sttChan
		ms.pipelineCancel = cancel
		ms.mu.Unlock()
	}

	ms.mu.Lock()
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
		ms.mu.Unlock()
		select {
		case sttChan <- data:
		default:
		}
	} else {
		ms.mu.Unlock()
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

	ms.mu.Lock()
	ms.sttRequestStartTime = time.Now()
	ms.mu.Unlock()
	result, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	ms.mu.Lock()
	if err == nil {
		ms.sttEndTime = time.Now()
		ms.lastNoSpeechProb = result.NoSpeechProb
	}
	ms.mu.Unlock()

	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	audioDuration := time.Since(ms.userSpeechStartTime)
	if !ms.userSpeechEndTime.IsZero() {
		audioDuration = ms.userSpeechEndTime.Sub(ms.userSpeechStartTime)
	}

	if result.Text == "" || ms.isLikelyNoise(result, audioDuration) {
		if result.Text != "" {
			fmt.Printf("\r\033[K🔄 [NOISE] Rejected hallucination: '%s' (prob=%.2f, dur=%v)\n", result.Text, result.NoSpeechProb, audioDuration)
		}
		ms.emit(BotResumed, nil)
		return
	}

	transcript := result.Text

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
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()

	defer rCancel()

	ms.emitWithGen(BotThinking, nil, gen)

	ms.mu.Lock()
	ms.llmStartTime = time.Now()
	ms.mu.Unlock()

	// Try streaming if supported
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
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()

	type pendingToolResult struct {
		tc     ToolCallEventData
		result string
	}
	var toolResults []pendingToolResult

	_, err := provider.StreamComplete(ctx, messages, ms.session.GetTools(), func(chunk string) error {
		fullText.WriteString(chunk)
		ms.mu.Lock()
		if ms.llmEndTime.IsZero() {
			ms.llmEndTime = time.Now()
		}
		ms.mu.Unlock()
		return nil
	}, func(tc ToolCallEventData) error {
		// If the model produced some text BEFORE the tool call (the "filler"), speak it immediately
		fillerText := strings.TrimSpace(fullText.String())
		if fillerText != "" && !hasToolCalls {
			go func(t string) {
				ttsCtx, ttsCancel := context.WithCancel(ctx)
				defer ttsCancel()
				ms.speakText(ttsCtx, t)
			}(fillerText)
			fullText.Reset()
		}

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
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("Streaming LLM error: %v", err))
		}
		return
	}

	response := strings.TrimSpace(fullText.String())

	if response != "" {
		// Only add to history now if there are NO tool calls.
		// If there are tool calls, we add it later along with the calls.
		if !hasToolCalls {
			ms.session.AddMessage("assistant", response)
		}
		ms.emit(BotResponse, response)

		// Create a local context for synthesis
		ttsCtx, ttsCancel := context.WithCancel(ctx)
		defer ttsCancel()
		ms.speakText(ttsCtx, response)
	} else {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
	}

	if hasToolCalls {
		// Add Tool Calls to History in correct sequence
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

		// This assistant message includes BOTH any text response and the tool calls
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

		// Recurse to handle the tool results
		go ms.runLLMAndTTS(ms.ctx, "")
	}
}

func (ms *ManagedStream) speakText(ctx context.Context, text string) {
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
	ms.audioBuf.Reset()
	ms.mu.Unlock()

	ms.emit(BotSpeaking, nil)

	err := ms.orch.SynthesizeStream(sCtx, text, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		ms.mu.Lock()
		ms.lastAudioSentAt = time.Now()
		if ms.ttsFirstChunkTime.IsZero() {
			ms.ttsFirstChunkTime = time.Now()
		}
		gen := ms.payloadGen
		ms.mu.Unlock()

		frameSize := 1764 // 44100Hz * 0.02s * 2 bytes
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

	if err != nil && sCtx.Err() == nil {
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
	ms.mu.Unlock()
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

	if ms.userSpeechEndTime.IsZero() {
		ms.userSpeechEndTime = time.Now()
	}
	ms.sttEndTime = ms.userSpeechEndTime // For latency breakdown consistency

	ms.isSpeaking = false
	ms.isThinking = false
	ms.userInterrupting = false
	// We don't increment gen here anymore as runLLMAndTTS handles it,
	// keeping it simple and unified.
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
