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
	lastInterruptedAt time.Time
	lastAudioSentAt   time.Time

	// New: response context for cancelling LLM/TTS independently of STT
	responseCancel context.CancelFunc
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

	// Dynamic Echo Guard: If we're currently or recently sent audio, increase VAD threshold
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		originalThreshold := rmsVAD.Threshold()
		if time.Since(ms.lastAudioSentAt) < 250*time.Millisecond {
			// Disable adaptive noise tracking while bot is speaking to prevent
			// the speaker output from "poisoning" our background noise floor estimate.
			rmsVAD.SetAdaptiveMode(false)
			rmsVAD.SetThreshold(0.25) // 0.25 is a better balance for interruption
			defer func() {
				rmsVAD.SetThreshold(originalThreshold)
				rmsVAD.SetAdaptiveMode(true)
			}()
		}
	}

	// 1. Process VAD
	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
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

	// Buffer audio and send to STT stream if active
	if ms.sttChan != nil {
		select {
		case ms.sttChan <- chunk:
		default:
			// Channel full
		}
	}

	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	// Buffer management with pre-roll:
	// If not speaking, keep only the last 500ms of audio as pre-roll lead-in.
	// 44100Hz * 2 bytes * 0.5s = 44100 bytes.
	ms.audioBuf.Write(chunk)
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

	// Flush pre-roll buffer to the new STT stream
	if ms.audioBuf.Len() > 0 {
		data := make([]byte, ms.audioBuf.Len())
		copy(data, ms.audioBuf.Bytes())
		select {
		case sttChan <- data:
		default:
			// If channel is full, we might lose the pre-roll, but we avoid blocking the write loop.
		}
	}
	ms.mu.Unlock()
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	ms.mu.Lock()
	ms.internalInterrupt()
	ctx, cancel := context.WithCancel(ms.ctx)
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
	// Cancel any existing response interaction to avoid overlapping audio
	if ms.responseCancel != nil {
		ms.responseCancel()
	}
	// Create a sub-context for this response interaction
	rCtx, rCancel := context.WithCancel(ctx)
	ms.responseCancel = rCancel
	ms.isThinking = true
	ms.mu.Unlock()

	defer rCancel()

	ms.emit(BotThinking, nil)

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	if err != nil {
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true
	// Clear VAD state right before speaking
	if ms.vad != nil {
		ms.vad.Reset()
	}
	ms.mu.Unlock()

	ms.emit(BotSpeaking, nil)

	err = ms.orch.SynthesizeStream(rCtx, response, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		select {
		case <-rCtx.Done():
			return rCtx.Err()
		default:
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.mu.Unlock()
			ms.emit(AudioChunk, chunk)
			return nil
		}
	})

	if err != nil && rCtx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	// Only clear responseCancel if it's still us
	// (Minor race here, but rCancel call is idempotent)
	ms.mu.Unlock()
}

// NotifyAudioPlayed should be called by the client/agent whenever it actually
// plays a chunk of audio to the speakers. This helps the Echo Guard
// correctly identify periods when it should be more resistant to barge-in.
func (ms *ManagedStream) NotifyAudioPlayed() {
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now()
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
	// If it's an audio chunk, only emit it if the bot is actually supposed to be speaking.
	// This prevents stale audio chunks from a previous generation (that was interrupted)
	// from leaking into the channel after an Interrupted event.
	if eventType == AudioChunk {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		ms.mu.Unlock()
		if !speaking {
			return
		}
	}

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

	// For AudioChunk, we should ideally never drop unless the stream is closed.
	// Dropping causes choppiness. We block until space is available or context is done.
	select {
	case ms.events <- event:
	case <-ms.ctx.Done():
		// Stream closed, stopping emission
	}
}

func (ms *ManagedStream) interrupt() {
	ms.mu.Lock()
	ms.internalInterrupt()
	ms.mu.Unlock()
}

// internalInterrupt handles the interruption logic without locking (caller must lock)
func (ms *ManagedStream) internalInterrupt() {
	// If we are already interrupted or not doing anything, ignore
	if !ms.isSpeaking && !ms.isThinking {
		return
	}

	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
		ms.pipelineCancel = nil
	}
	if ms.responseCancel != nil {
		ms.responseCancel()
		ms.responseCancel = nil
	}

	ms.lastInterruptedAt = time.Now()
	ms.isSpeaking = false
	ms.isThinking = false

	// Clear the events channel of any pending AudioChunks to ensure
	// the Interrupted event is processed as soon as possible by the client.
	ms.drainAudioChunks()

	ms.emit(Interrupted, nil)

	if ms.sttChan != nil {
		ms.sttChan = nil
	}
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
