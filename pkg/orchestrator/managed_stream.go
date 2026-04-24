package orchestrator

import (
	"bytes"
	"context"
	"fmt"
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
	lastTranscript string // Tracks the latest user transcription
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

	payloadGen       int
	writeChan        chan []byte
	isClosed         bool
	lastNoSpeechProb float64
	inPreemptiveTurn bool
	lastActivityAt   time.Time
	playbackRate     int

	toolRecursionDepth int // Safety counter to prevent infinite tool loops
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
		playbackRate:   44100, // Default to hifi
		turnCompletion: NewTurnCompletionAnalyzer(),
	}

	go ms.processBackgroundAudio()
	go ms.monitorInactivity()

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
			if ms.userSpeechStartTime.IsZero() {
				ms.userSpeechStartTime = time.Now()
			}
			ms.mu.Unlock()

			// We now emit UserSpeaking on a confirmed start to prevent glitchy pausing
			ms.emit(UserSpeaking, nil)

			ms.mu.Lock()
			ms.sttGeneration++
			pipelineCancel := ms.pipelineCancel
			sttChan := ms.sttChan
			ttsCancel := ms.ttsCancel
			ms.pipelineCancel = nil
			ms.sttChan = nil
			ms.ttsCancel = nil

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
			ms.mu.Unlock()

			// Stop TTS immediately on interrupt
			if ttsCancel != nil {
				ttsCancel()
			}
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
				ms.mu.Unlock()

				go func(buf []byte) {
					ms.mu.Lock()
					duration := ms.userSpeechEndTime.Sub(ms.userSpeechStartTime)
					lastTranscript := ms.lastTranscript
					ms.mu.Unlock()

					// Fast-path: if the sound was very short, don't wait for another second
					// to see if the user continues. It's likely just noise.
					if duration < 500*time.Millisecond {
						ms.runBatchPipeline(buf)
						return
					}

					// Adaptive hold time: longer hold for short utterances (likely pauses),
					// shorter hold for longer utterances (likely complete thoughts)
					completionScore := ms.turnCompletion.CombinedCompletionScore(
						lastTranscript,
						int(duration.Milliseconds()),
						ms.vad,
					)

					// SMART HOLD: completion score gates the response speed.
					// High score = user finished sentence → respond FAST.
					// Low score = user paused mid-sentence → wait LONG.
					var holdTime time.Duration
					if completionScore < 0.35 {
						// Incomplete sentence (e.g. "I think that...") → long hold
						holdTime = 600 * time.Millisecond
					} else if completionScore > 0.65 {
						// Complete sentence (e.g. "How are you?") → respond immediately
						holdTime = 50 * time.Millisecond
					} else {
						// Ambiguous → medium hold, shorter for longer utterances
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
						// FIX: Check IsSpeaking() via the generic interface so this
						// works for ALL VAD types, not just ImprovedRMSVAD.
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
	}

	cleanChunk := chunk
	// Protect against byte-tearing on S16 PCM chunks
	if len(cleanChunk)%2 != 0 {
		cleanChunk = cleanChunk[:len(cleanChunk)-1]
	}

	ms.mu.Lock()
	ms.audioBuf.Write(cleanChunk)
	// Keep maximum 4 seconds of pristine audio (176400 bytes). Must slice on an EVEN byte boundary (2-byte samples).
	// Crucially, only trim if we are NOT in the middle of a turn.
	if !isUserSpeaking && ms.userSpeechStartTime.IsZero() && ms.audioBuf.Len() > 176400 {
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

func (ms *ManagedStream) isLikelyNoise(result TranscriptionResult, audioDuration time.Duration) bool {
	// If the STT engine is >= 70% sure this is not speech, trust it.
	if result.NoSpeechProb > 0.7 {
		return true
	}

	clean := strings.TrimSpace(result.Text)
	if clean == "" {
		return true
	}

	// Only reject very short audio with extremely short transcription.
	// "Yes", "No", "Hi", "Okay" are all valid conversational utterances
	// that must not be discarded. Only reject 1-2 character transcripts
	// under 300ms (likely clicks/breaths), or empty text.
	if audioDuration < 300*time.Millisecond && len(clean) <= 1 {
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
		// Track transcript for turn completion analysis
		ms.lastTranscript = transcript
		ms.mu.Unlock()

		if isStale && !isFinal {
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

	if result.Text == "" || ms.isLikelyNoise(result, audioDuration) {
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
	speaking := ms.isSpeaking
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

	// Reset tool recursion depth on new user turn (when transcript is non-empty)
	if transcript != "" {
		ms.toolRecursionDepth = 0
	}

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
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()
	fmt.Printf("\r\033[K[DEBUG] runStreamingLLM: Starting with %d messages in session\n", len(messages))

	// Debug: Show message breakdown
	var systemCount, userCount, assistantCount, toolCount int
	for _, m := range messages {
		switch m.Role {
		case "system":
			systemCount++
		case "user":
			userCount++
		case "assistant":
			assistantCount++
		case "tool":
			toolCount++
		}
	}
	fmt.Printf("\r\033[K[DEBUG] Message breakdown: system=%d, user=%d, assistant=%d, tool=%d\n", systemCount, userCount, assistantCount, toolCount)

	// Debug: Show last 3 messages for context
	if len(messages) > 0 {
		fmt.Printf("\r\033[K[DEBUG] Last messages in context:\n")
		start := len(messages) - 3
		if start < 0 {
			start = 0
		}
		for i := start; i < len(messages); i++ {
			m := messages[i]
			content := m.Content
			if len(content) > 60 {
				content = content[:60] + "..."
			}
			if m.Role == "tool" {
				fmt.Printf("\r\033[K  [%d] %s (id=%s): %s\n", i, m.Role, m.ToolCallID, content)
			} else if m.ToolCalls != nil {
				fmt.Printf("\r\033[K  [%d] %s (with tool calls): %s\n", i, m.Role, content)
			} else {
				fmt.Printf("\r\033[K  [%d] %s: %s\n", i, m.Role, content)
			}
		}
	}

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
		fmt.Printf("\r\033[K[DEBUG] Tool call #%d: %s, callID=%s\n", toolCallCount, tc.Name, tc.CallID)

		// If the model produced some text BEFORE the tool call (the "filler"), speak it immediately
		fillerText := strings.TrimSpace(fullText.String())
		if fillerText != "" && !hasToolCalls {
			fmt.Printf("\r\033[K[DEBUG] Speaking filler text before tool call: %q\n", fillerText)
			go func(t string) {
				ttsCtx, ttsCancel := context.WithCancel(ctx)
				defer ttsCancel()
				ms.speakText(ttsCtx, t)
			}(fillerText)
			fullText.Reset()
		}

		hasToolCalls = true
		fmt.Printf("\r\033[K[DEBUG] Tool call detected: %s, callID=%s\n", tc.Name, tc.CallID)
		ms.emit(ToolCall, tc)

		o := ms.orch
		o.mu.RLock()
		handler, ok := o.toolHandlers[tc.Name]
		o.mu.RUnlock()

		result := "Error: tool not found"
		if ok {
			fmt.Printf("\r\033[K[DEBUG] Executing tool: %s with args: %v\n", tc.Name, tc.Arguments)
			var err error
			result, err = handler(tc.Arguments)
			if err != nil {
				result = fmt.Sprintf("Error: %v", err)
			}
			fmt.Printf("\r\033[K[DEBUG] Tool result: %q\n", result)
		}

		toolResults = append(toolResults, pendingToolResult{tc: tc, result: result})
		return nil
	})

	if err != nil {
		ms.mu.Lock()
		ms.isThinking = false
		ms.mu.Unlock()
		if ctx.Err() == nil {
			fmt.Printf("\r\033[K[DEBUG] Streaming LLM error: %v\n", err)
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
		fmt.Printf("\r\033[K[DEBUG] Adding %d tool results to session history\n", len(toolResults))
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
		fmt.Printf("\r\033[K[DEBUG] Recursing to process tool results (depth=%d)\n", ms.toolRecursionDepth)
		ms.mu.Lock()
		ms.toolRecursionDepth++
		depth := ms.toolRecursionDepth
		ms.mu.Unlock()

		// Safety check: prevent infinite loops from tool recursion
		if depth > 3 {
			fmt.Printf("\r\033[K[DEBUG] ⚠️  SAFETY: Tool recursion depth exceeded (depth=%d), stopping recursion and speaking accumulated response\n", depth)
			// Don't recurse further, just ensure the bot can speak whatever response we have
			ms.mu.Lock()
			ms.isThinking = false
			ms.mu.Unlock()
			return
		}

		// Use a fresh context for the recursive call rather than the streaming context
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

	// Check if there's anything to interrupt (TTS or LLM request)
	// We allow a 1-second window after isSpeaking=false to account for audio in the playback buffer.
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
