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
	pipelineCtx     context.Context
	pipelineCancel  context.CancelFunc
	sttChan         chan<- []byte
	isSpeaking      bool
	isThinking      bool
	lastAudioSentAt time.Time
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
		events:   make(chan OrchestratorEvent, 100),
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
		// If we sent audio in the last 1 second, be much less sensitive
		if time.Since(ms.lastAudioSentAt) < 1000*time.Millisecond {
			// Save the original threshold if we haven't already?
			// Actually, just set it to a high "echo-guard" value
			rmsVAD.SetThreshold(0.20)
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

	if event != nil {
		switch event.Type {
		case VADSpeechStart:
			ms.emit(UserSpeaking, nil)
			// Interrupt bot if it was speaking or thinking
			ms.internalInterrupt()
			ms.audioBuf.Reset()

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

	// Buffer audio and send to STT stream if active
	if ms.sttChan != nil {
		select {
		case ms.sttChan <- chunk:
		default:
			// Channel full, handle or log
		}
	}
	ms.audioBuf.Write(chunk)

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

	// For AudioChunk, we can drop if the buffer is too full to prevent lag,
	// or we can just send. Usually dropping audio is better than 2s lag.
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
