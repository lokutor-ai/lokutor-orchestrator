package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"
)

type SpeculativeState int

const (
	SpecIdle SpeculativeState = iota
	SpecRunning
	SpecReady
)

type SpeculativeResult struct {
	PartialTranscript string
}

type SpeculativeExecutor struct {
	mu         sync.Mutex
	state      SpeculativeState
	interval   time.Duration
	lastSpecAt time.Time
	result     *SpeculativeResult
	cancel     context.CancelFunc

	onPartial func(transcript string)
}

func NewSpeculativeExecutor(intervalMs int) *SpeculativeExecutor {
	if intervalMs <= 0 {
		intervalMs = 400
	}
	return &SpeculativeExecutor{
		interval: time.Duration(intervalMs) * time.Millisecond,
	}
}

func (se *SpeculativeExecutor) SetOnPartial(cb func(transcript string)) {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.onPartial = cb
}

func (se *SpeculativeExecutor) ShouldSpeculate(speechDuration time.Duration, lastSpecAt time.Time) bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.state != SpecIdle {
		return false
	}
	if speechDuration < 1500*time.Millisecond {
		return false
	}
	if !lastSpecAt.IsZero() && time.Since(lastSpecAt) < se.interval {
		return false
	}
	return true
}

func (se *SpeculativeExecutor) Start(ctx context.Context, orch *Orchestrator, audio []byte, lang Language) {
	se.mu.Lock()
	if se.state != SpecIdle {
		se.mu.Unlock()
		return
	}
	se.state = SpecRunning
	sCtx, sCancel := context.WithTimeout(ctx, 5*time.Second)
	se.cancel = sCancel
	onPartial := se.onPartial
	se.mu.Unlock()

	go func() {
		defer sCancel()
		defer func() {
			if r := recover(); r != nil {
				se.mu.Lock()
				se.state = SpecIdle
				se.cancel = nil
				se.mu.Unlock()
			}
		}()

		result, err := orch.TranscribeRaw(sCtx, audio, lang)
		if err != nil || sCtx.Err() != nil {
			se.mu.Lock()
			if se.state == SpecRunning {
				se.state = SpecIdle
				se.cancel = nil
			}
			se.mu.Unlock()
			return
		}

		partial := strings.TrimSpace(result.Text)
		if partial == "" || len(partial) < 2 {
			se.mu.Lock()
			if se.state == SpecRunning {
				se.state = SpecIdle
				se.cancel = nil
			}
			se.mu.Unlock()
			return
		}

		// Emit partial transcript for client-side display
		if onPartial != nil {
			onPartial(partial)
		}

		se.mu.Lock()
		if se.state == SpecRunning {
			se.state = SpecIdle
			se.result = &SpeculativeResult{PartialTranscript: partial}
		}
		se.cancel = nil
		se.mu.Unlock()
	}()
}

func (se *SpeculativeExecutor) Cancel() {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.cancel != nil {
		se.cancel()
		se.cancel = nil
	}
	se.state = SpecIdle
	se.result = nil
}
