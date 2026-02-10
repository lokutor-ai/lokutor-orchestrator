package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"
)

// ManagedStream handles full-duplex voice orchestration
type ManagedStream struct {
	orch    *Orchestrator
	session *ConversationSession
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan OrchestratorEvent
	vad     VADProvider

	audioBuf *bytes.Buffer
	mu       sync.Mutex

	// Pipeline control
	pipelineCtx       context.Context
	pipelineCancel    context.CancelFunc
	sttChan           chan<- []byte
	isSpeaking        bool
	isThinking        bool
	lastAudioSentAt   time.Time
	lastInterruptedAt time.Time
}

// NewManagedStream creates a new managed stream
func NewManagedStream(ctx context.Context, o *Orchestrator, session *ConversationSession) *ManagedStream {
	mCtx, mCancel := context.WithCancel(ctx)

	var streamVAD VADProvider
	if o.vad != nil {
		streamVAD = o.vad.Clone()
	}

	ms := &ManagedStream{
		orch:     o,
		session:  session,
		ctx:      mCtx,
		cancel:   mCancel,
		events:   make(chan OrchestratorEvent, 1024),
		audioBuf: new(bytes.Buffer),
		vad:      streamVAD,
	}

	return ms
}

// Write adds audio data to the stream and processes it through the VAD
func (ms *ManagedStream) Write(chunk []byte) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.vad == nil {
		return fmt.Errorf("VAD not configured for this stream")
	}

	// Adaptive Echo Guard: If we recently sent audio, we might be hearing our own echo.
	// We temporarily increase the threshold if using RMSVAD.
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		// If the bot is CURRENTLY emitting audio chunks, we lock the threshold to a total-mute level.
		// We use a generous 1s window to account for network jitter and buffering.
		isStillEmitting := time.Since(ms.lastAudioSentAt) < 1000*time.Millisecond
		isRecentlyEmitted := time.Since(ms.lastAudioSentAt) < 2000*time.Millisecond

		if isStillEmitting {
			// Total mute level while bot is actively pushing data
			rmsVAD.SetThreshold(0.95)
		} else if isRecentlyEmitted {
			// Handling the acoustic tail/reverb
			rmsVAD.SetThreshold(0.65)
		} else {
			// Restore to base threshold (configured in NewWithVAD/Orchestrator)
			if baseVAD, baseOk := ms.orch.vad.(*RMSVAD); baseOk {
				rmsVAD.SetThreshold(baseVAD.Threshold())
			}
		}
	}

	// 1. Process VAD
	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	if event != nil {
		switch event.Type {
		case VADSpeechStart:
			ms.emit(UserSpeaking, nil)
			// Interrupt bot if it was speaking or thinking
			ms.internalInterrupt()

			// Start streaming STT if supported
			if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
				ms.startStreamingSTT(sProvider)
			}

		case VADSpeechEnd:
			ms.emit(UserStopped, nil)

			if ms.sttChan != nil {
				close(ms.sttChan)
				ms.sttChan = nil
			} else {
				// Fallback to batch STT if not already streaming
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				go ms.runBatchPipeline(audioData)
			}

		case VADSilence:
			// Just silence
		}
	}

	// Dynamic Echo Blanking:
	// If we are in the echo danger zone and speech hasn't been confirmed yet,
	// we zero out the chunk to prevent "Ghost Audio" (echo being used as prompt).
	isRecentlyEmitted := time.Since(ms.lastAudioSentAt) < 1500*time.Millisecond
	processedChunk := chunk
	if isRecentlyEmitted && !isUserSpeaking {
		processedChunk = make([]byte, len(chunk))
	}

	// Buffer audio and send to STT stream if active
	if ms.sttChan != nil {
		select {
		case ms.sttChan <- processedChunk:
		default:
			// Channel full, handle or log
		}
	}

	// Buffer management with pre-roll:
	// If not speaking, keep only the last 500ms of audio as pre-roll lead-in.
	// 44100Hz * 2 bytes * 0.5s = 44100 bytes.
	ms.audioBuf.Write(processedChunk)
	if !isUserSpeaking && ms.audioBuf.Len() > 50000 {
		// Trim to keep ~500ms
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-44100:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}

	return nil
}

func (ms *ManagedStream) startStreamingSTT(provider StreamingSTTProvider) {
	// Create context for this interaction
	ctx, cancel := context.WithCancel(ms.ctx)

	sttChan, err := provider.StreamTranscribe(ctx, ms.session.GetCurrentLanguage(), func(transcript string, isFinal bool) error {
		if isFinal {
			ms.emit(TranscriptFinal, transcript)
			ms.session.AddMessage("user", transcript)
			// Start LLM -> TTS pipeline
			go ms.runLLMAndTTS(ctx, transcript)
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

	ms.mu.Lock()
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttChan = sttChan
	ms.mu.Unlock()
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	ctx, cancel := context.WithCancel(ms.ctx)
	ms.mu.Lock()
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.mu.Unlock()
	defer cancel()

	ms.emit(BotThinking, nil)

	transcript, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	if transcript == "" {
		return
	}

	ms.emit(TranscriptFinal, transcript)
	ms.session.AddMessage("user", transcript)

	ms.runLLMAndTTS(ctx, transcript)
}

func (ms *ManagedStream) runLLMAndTTS(ctx context.Context, transcript string) {
	ms.mu.Lock()
	ms.isThinking = true
	ms.mu.Unlock()

	ms.emit(BotThinking, nil)

	response, err := ms.orch.GenerateResponse(ctx, ms.session)
	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true
	// Initialize lastAudioSentAt to now so the adaptive threshold kicks in immediately
	ms.lastAudioSentAt = time.Now()
	// Clear VAD state right before speaking to ensure pre-existing echo/noise 
	// doesn't trigger a barge-in immediately.
	if ms.vad != nil {
		ms.vad.Reset()
	}
	ms.mu.Unlock()

	ms.emit(BotSpeaking, nil)

	err = ms.orch.SynthesizeStream(ctx, response, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.mu.Unlock()
			ms.emit(AudioChunk, chunk)
			return nil
		}
	})

	if err != nil && ctx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	ms.mu.Unlock()
}

// Events returns the event channel
func (ms *ManagedStream) Events() <-chan OrchestratorEvent {
	return ms.events
}

// Close closes the managed stream
func (ms *ManagedStream) Close() {
	ms.cancel()
	ms.interrupt()
	close(ms.events)
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
	event := OrchestratorEvent{
		Type:      eventType,
		SessionID: ms.session.ID,
		Data:      data,
	}

	// Control events should never be dropped and should be sent as priority if possible.
	// Since Go channels are FIFO, we just ensure they aren't dropped.
	isControl := eventType != AudioChunk

	if isControl {
		select {
		case ms.events <- event:
		case <-ms.ctx.Done():
		}
		return
	}

	// For AudioChunk, we drop if the buffer is too full to prevent lag.
	// We have a 1024 buffer which is ~24s of audio, so if it's full,
	// the client is severely lagging and we MUST drop to maintain real-time.
	select {
	case ms.events <- event:
	default:
		// Channel full, dropping audio chunk to maintain real-time performance
	}
}

func (ms *ManagedStream) interrupt() {
	ms.mu.Lock()
	ms.internalInterrupt()
	ms.mu.Unlock()
}

// internalInterrupt handles the interruption logic without locking (caller must lock)
func (ms *ManagedStream) internalInterrupt() {
	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
		ms.pipelineCancel = nil

		ms.lastInterruptedAt = time.Now()

		// Clear the events channel of any pending AudioChunks to ensure
		// the Interrupted event is processed as soon as possible by the client.
		ms.drainAudioChunks()

		ms.emit(Interrupted, nil)
	}
	if ms.sttChan != nil {
		ms.sttChan = nil
	}
	ms.isSpeaking = false
	ms.isThinking = false
}

// drainAudioChunks removes all AudioChunk events from the events channel
func (ms *ManagedStream) drainAudioChunks() {
	var controlEvents []OrchestratorEvent
DrainLoop:
	for {
		select {
		case ev := <-ms.events:
			if ev.Type != AudioChunk {
				controlEvents = append(controlEvents, ev)
			}
		default:
			break DrainLoop
		}
	}
	// Re-insert control events
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
			// If we can't re-insert, we're in trouble, but the channel
			// was just drained so it should have space.
		}
	}
}
