package orchestrator

import (
	"math"
	"os"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/turno"
)

// Backchannel predictor in SHADOW (2026-11-19): a learned model of where a human listener would backchannel, run on the
// caller's audio 20 ms at a time beside the current pause rule, logging where it would fire so the two can be
// compared on real calls before the trigger is switched (lokutor_tts tools/backchannel_predictor). It never plays
// anything. Config.BackchannelPredictorPath names its ONNX model; only streams with backchannels on run it.

const bcShadowRefractory = 1500 * time.Millisecond

type bcShadow struct {
	p        *turno.BCPredictor
	thr      float32
	pending  []float32 // 16 kHz samples not yet a whole frame
	lastFire time.Time
	frames   int
	fires    int
	errs     int
	peak     float32
}

func (ms *ManagedStream) startBackchannelShadow(cfg Config) {
	if !cfg.Backchannels || cfg.BackchannelPredictorPath == "" {
		return
	}
	if _, err := os.Stat(cfg.BackchannelPredictorPath); err != nil {
		ms.logger.Info("Backchannel predictor (shadow): model not found, not running", "path", cfg.BackchannelPredictorPath)
		return
	}
	p, err := turno.NewBCPredictor(cfg.BackchannelPredictorPath)
	if err != nil {
		ms.logger.Warn("Backchannel predictor (shadow): could not load, not running", "error", err.Error())
		return
	}
	thr := float32(cfg.BackchannelPredictorThreshold)
	if thr <= 0 || thr >= 1 {
		thr = 0.5
	}
	ms.bcShadow = &bcShadow{p: p, thr: thr}
	ms.logger.Info("Backchannel predictor (shadow) loaded", "path", cfg.BackchannelPredictorPath, "threshold", thr)
}

// backchannelShadowAudio feeds 16 kHz PCM16 caller audio to the predictor and logs where it would fire.
func (ms *ManagedStream) backchannelShadowAudio(pcm16k []byte, now time.Time) {
	s := ms.bcShadow
	if s == nil {
		return
	}
	for i := 0; i+1 < len(pcm16k); i += 2 {
		s.pending = append(s.pending, float32(int16(uint16(pcm16k[i])|uint16(pcm16k[i+1])<<8))/32768)
	}
	for len(s.pending) >= turno.Hop {
		frame := s.pending[:turno.Hop]
		pLex, pAny, err := s.p.Step(frame)
		s.pending = s.pending[turno.Hop:]
		s.frames++
		if err != nil {
			s.errs++
			continue
		}
		if pLex > s.peak {
			s.peak = pLex
		}
		if pLex < s.thr || (!s.lastFire.IsZero() && now.Sub(s.lastFire) < bcShadowRefractory) {
			continue
		}
		s.lastFire = now
		s.fires++
		ms.mu.Lock()
		voiced, playing := ms.lastVoicedAt, ms.playout.end.After(now)
		ms.mu.Unlock()
		ms.bc.mu.Lock()
		rule := ms.bc.lastAt
		ms.bc.mu.Unlock()
		sinceRule := int64(-1)
		if !rule.IsZero() {
			sinceRule = now.Sub(rule).Milliseconds()
		}
		ms.logger.Info("Backchannel predictor (shadow) would fire", "p_lexical", round3(pLex), "p_any", round3(pAny),
			"since_voiced_ms", now.Sub(voiced).Milliseconds(), "agent_audio_playing", playing, "since_rule_ms", sinceRule)
	}
	if cap(s.pending) > 8*turno.Hop {
		s.pending = append([]float32(nil), s.pending...)
	}
}

func round3(f float32) float64 { return math.Round(float64(f)*1000) / 1000 }

func (ms *ManagedStream) stopBackchannelShadow() {
	s := ms.bcShadow
	if s == nil {
		return
	}
	ms.bc.mu.Lock()
	plays := ms.bc.plays
	ms.bc.mu.Unlock()
	ms.logger.Info("Backchannel predictor (shadow) call summary", "frames", s.frames, "shadow_fires", s.fires,
		"rule_plays", plays, "errors", s.errs, "peak_p_lexical", round3(s.peak), "threshold", s.thr)
	s.p.Destroy()
}
