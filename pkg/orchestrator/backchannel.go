package orchestrator

import (
	"sync"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
)

type BackchannelState int

const (
	bcIdle BackchannelState = iota
	bcListening
	bcLowPitch
)

type BackchannelConfig struct {
	MinSpeechDuration   time.Duration
	MinInterval         time.Duration
	LowPitchThresholdHz float64
	LowPitchPlateauMs   int
	PitchWindowFrames   int
}

func DefaultBackchannelConfig() BackchannelConfig {
	return BackchannelConfig{
		MinSpeechDuration:   2 * time.Second,
		MinInterval:         4 * time.Second,
		LowPitchThresholdHz: 220.0,
		LowPitchPlateauMs:   120,
		PitchWindowFrames:   10,
	}
}

type BackchannelDetector struct {
	mu         sync.Mutex
	cfg        BackchannelConfig
	sampleRate int

	state             BackchannelState
	userSpeakingSince time.Time
	lastBackchannel   time.Time
	pitchHistory      []float64
	plateauStart      time.Time
	lastPitchTime     time.Time

	onBackchannel func([]byte)
	clips         [][]byte
}

func NewBackchannelDetector(cfg BackchannelConfig, sampleRate int, onBackchannel func([]byte)) *BackchannelDetector {
	return &BackchannelDetector{
		cfg:           cfg,
		sampleRate:    sampleRate,
		state:         bcIdle,
		pitchHistory:  make([]float64, 0, cfg.PitchWindowFrames),
		onBackchannel: onBackchannel,
	}
}

func (bd *BackchannelDetector) UserStarted() {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.userSpeakingSince = time.Now()
	bd.state = bcListening
	bd.pitchHistory = bd.pitchHistory[:0]
}

func (bd *BackchannelDetector) UserStopped() {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.state = bcIdle
	bd.pitchHistory = bd.pitchHistory[:0]
}

func (bd *BackchannelDetector) ProcessAudio(chunk []int16, now time.Time) {
	bd.mu.Lock()
	defer bd.mu.Unlock()

	if bd.state == bcIdle {
		return
	}

	speechDuration := now.Sub(bd.userSpeakingSince)
	if speechDuration < bd.cfg.MinSpeechDuration {
		return
	}

	pitch := audio.DetectPitch(chunk, bd.sampleRate)

	windowMs := int(float64(len(chunk)) / float64(bd.sampleRate) * 1000)
	if windowMs < 1 {
		windowMs = 20
	}

	// Track pitch in a sliding history
	if pitch > 0 {
		bd.pitchHistory = append(bd.pitchHistory, pitch)
		if len(bd.pitchHistory) > bd.cfg.PitchWindowFrames {
			bd.pitchHistory = bd.pitchHistory[1:]
		}
	} else {
		bd.pitchHistory = append(bd.pitchHistory, 0)
		if len(bd.pitchHistory) > bd.cfg.PitchWindowFrames {
			bd.pitchHistory = bd.pitchHistory[1:]
		}
	}

	// Check for sustained low pitch
	lowPitchCount := 0
	for _, p := range bd.pitchHistory {
		if p > 0 && p < bd.cfg.LowPitchThresholdHz {
			lowPitchCount++
		}
	}

	allLow := lowPitchCount >= len(bd.pitchHistory)/2

	switch bd.state {
	case bcListening:
		if allLow && pitch > 0 {
			bd.state = bcLowPitch
			bd.plateauStart = now
			bd.lastPitchTime = now
		}
	case bcLowPitch:
		if !allLow || pitch == 0 {
			bd.state = bcListening
			bd.plateauStart = time.Time{}
			break
		}

		bd.lastPitchTime = now
		plateauMs := int(now.Sub(bd.plateauStart).Milliseconds())

		if plateauMs >= bd.cfg.LowPitchPlateauMs {
			sinceLast := now.Sub(bd.lastBackchannel)
			if sinceLast >= bd.cfg.MinInterval {
				bd.fire()
			}
			bd.state = bcListening
			bd.plateauStart = time.Time{}
		}
	}
}

func (bd *BackchannelDetector) fire() {
	if bd.onBackchannel == nil {
		return
	}

	bd.lastBackchannel = time.Now()

	// Use pre-generated clips if available, otherwise fall back to programmatic
	var raw []byte
	if len(bd.clips) > 0 {
		idx := bd.lastBackchannel.UnixMilli() % int64(len(bd.clips))
		raw = bd.clips[idx]
	} else {
		sound := audio.GenerateBackchannel(audio.BackchannelMhm, bd.sampleRate)
		raw = make([]byte, len(sound)*2)
		for i, s := range sound {
			raw[i*2] = byte(s)
			raw[i*2+1] = byte(s >> 8)
		}
	}

	go bd.onBackchannel(raw)
}

func (bd *BackchannelDetector) SetClips(clips [][]byte) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.clips = clips
}
