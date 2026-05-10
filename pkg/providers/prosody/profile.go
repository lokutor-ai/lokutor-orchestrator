package prosody

import (
	"math"
	"sync"
)

type UserSpeechProfile struct {
	mu sync.Mutex

	// Running stats from user speech
	speakingRates   []float64 // words per second
	avgEnergy       float64
	turnDurationsMs []int

	// Current best estimate
	estimatedRate  float64 // words/sec
	estimatedEnergy float64
	estimatedPauseMs int

	// Adaptation rate (0-1): how fast we adjust to new data
	adaptRate float64

	// Whether we have enough data
	hasBaseline bool
}

func NewUserSpeechProfile() *UserSpeechProfile {
	return &UserSpeechProfile{
		speakingRates:   make([]float64, 0, 20),
		turnDurationsMs: make([]int, 0, 20),
		adaptRate:       0.3,
	}
}

func (p *UserSpeechProfile) RecordUtterance(wordCount int, durationMs int, avgRMS float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if durationMs <= 0 || wordCount <= 0 {
		return
	}

	rate := float64(wordCount) / (float64(durationMs) / 1000.0)

	p.speakingRates = append(p.speakingRates, rate)
	p.turnDurationsMs = append(p.turnDurationsMs, durationMs)

	// Keep a sliding window of last 10 utterances
	maxHistory := 10
	if len(p.speakingRates) > maxHistory {
		p.speakingRates = p.speakingRates[1:]
	}
	if len(p.turnDurationsMs) > maxHistory {
		p.turnDurationsMs = p.turnDurationsMs[1:]
	}

	// Smooth the rate estimate using exponential moving average
	if !p.hasBaseline {
		p.estimatedRate = rate
		p.estimatedEnergy = avgRMS
		p.hasBaseline = true
	} else {
		p.estimatedRate += p.adaptRate * (rate - p.estimatedRate)
		p.estimatedEnergy += p.adaptRate * (avgRMS - p.estimatedEnergy)
	}

	// Estimate pause duration from turn duration
	// Shorter utterances tend to have shorter pauses between them
	avgDur := 0
	for _, d := range p.turnDurationsMs {
		avgDur += d
	}
	if len(p.turnDurationsMs) > 0 {
		avgDur /= len(p.turnDurationsMs)
	}
	p.estimatedPauseMs = int(float64(avgDur) * 0.15)
	if p.estimatedPauseMs < 200 {
		p.estimatedPauseMs = 200
	}
	if p.estimatedPauseMs > 800 {
		p.estimatedPauseMs = 800
	}
}

func (p *UserSpeechProfile) GetSuggestedSpeechRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasBaseline {
		return 1.0
	}
	// Map user rate to TTS rate
	// Normal conversation: 2.5-5.0 words/sec → TTS rate 0.85-1.25
	clamped := math.Max(1.5, math.Min(6.0, p.estimatedRate))
	mapped := 0.7 + (clamped-1.5)*(0.6/4.5)
	return math.Max(0.7, math.Min(1.3, mapped))
}

func (p *UserSpeechProfile) GetSuggestedResponseLatency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasBaseline {
		return 200
	}
	// Faster talkers should get faster responses
	threshold := 3.5 // words/sec
	if p.estimatedRate > threshold {
		return 80
	}
	// Slower talkers get more breathing room
	return p.estimatedPauseMs
}

func (p *UserSpeechProfile) GetSuggestedEmphasisLevel() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasBaseline {
		return 0.5
	}
	// Higher energy users get more emphasis from the bot
	normEnergy := math.Min(1.0, p.estimatedEnergy/15000.0)
	return 0.3 + normEnergy*0.5
}

func (p *UserSpeechProfile) Flatten() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hasBaseline = false
	p.estimatedRate = 0
	p.estimatedEnergy = 0
	p.speakingRates = p.speakingRates[:0]
	p.turnDurationsMs = p.turnDurationsMs[:0]
}

func (p *UserSpeechProfile) HasBaseline() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasBaseline
}
