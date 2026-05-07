package orchestrator

import (
	"context"
	"math"
	"sync"
	"time"
)

type BackchannelGenerator struct {
	mu sync.Mutex

	enabled      bool
	threshold    float64
	language     Language
	orch         *Orchestrator
	session      *ConversationSession

	lastBackchannel time.Time
	backchannelGap  time.Duration

	userSpeechStart time.Time
	userPauseStart  time.Time
	isInPause       bool
	energyWindow    []float64
	windowIdx       int

	userBaselineRMS float64
	baselineSamples int

	cooldownUntil time.Time

	cachedAudio   map[string][]byte
	synthesisLock sync.Mutex
}

func NewBackchannelGenerator(orch *Orchestrator, session *ConversationSession, enabled bool, threshold float64, lang Language) *BackchannelGenerator {
	return &BackchannelGenerator{
		enabled:         enabled,
		threshold:       threshold,
		language:        lang,
		orch:            orch,
		session:         session,
		backchannelGap:  3 * time.Second,
		energyWindow:    make([]float64, 5),
		userBaselineRMS: 0.02,
	}
}

func (bg *BackchannelGenerator) RecordUserSpeechStart() {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.userSpeechStart = time.Now()
	bg.isInPause = false
	bg.userPauseStart = time.Time{}
}

func (bg *BackchannelGenerator) RecordAudio(rms float64) {
	bg.mu.Lock()
	defer bg.mu.Unlock()

	if rms <= 0 {
		return
	}

	if bg.baselineSamples < 50 {
		bg.userBaselineRMS = (bg.userBaselineRMS*float64(bg.baselineSamples) + rms) / float64(bg.baselineSamples+1)
		bg.baselineSamples++
	} else {
		bg.userBaselineRMS = bg.userBaselineRMS*0.98 + rms*0.02
	}

	bg.energyWindow[bg.windowIdx] = rms
	bg.windowIdx = (bg.windowIdx + 1) % len(bg.energyWindow)
}

func (bg *BackchannelGenerator) OnUserPause() (shouldBackchannel bool, backchannelText string) {
	bg.mu.Lock()
	defer bg.mu.Unlock()

	if !bg.enabled {
		return false, ""
	}

	if time.Now().Before(bg.cooldownUntil) {
		return false, ""
	}

	if !bg.userPauseStart.IsZero() {
		pauseDuration := time.Since(bg.userPauseStart)
		if pauseDuration > 250*time.Millisecond && pauseDuration < 800*time.Millisecond {
			userSpeechDuration := time.Since(bg.userSpeechStart)
			if userSpeechDuration < 800*time.Millisecond {
				return false, ""
			}

			energyTrend := bg.energyTrend()
			if energyTrend < -0.02 {
				bg.cooldownUntil = time.Now().Add(bg.backchannelGap)
				return true, bg.selectBackchannel()
			}
		}
	}
	bg.userPauseStart = time.Now()
	bg.isInPause = true

	return false, ""
}

func (bg *BackchannelGenerator) OnUserResumed() {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.userPauseStart = time.Time{}
	bg.isInPause = false
}

func (bg *BackchannelGenerator) OnSpeechEnd() {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.userPauseStart = time.Time{}
	bg.isInPause = false
}

func (bg *BackchannelGenerator) energyTrend() float64 {
	n := 0
	sumX := 0.0
	sumY := 0.0
	sumXY := 0.0
	sumX2 := 0.0

	for i, val := range bg.energyWindow {
		if val > 0 {
			x := float64(i)
			n++
			sumX += x
			sumY += val
			sumXY += x * val
			sumX2 += x * x
		}
	}

	if n < 2 {
		return 0
	}

	nf := float64(n)
	return (nf*sumXY - sumX*sumY) / (nf*sumX2 - sumX*sumX)
}

func (bg *BackchannelGenerator) selectBackchannel() string {
	switch bg.language {
	case LanguageEs:
		return shortBackchannelES[bg.fastRand(len(shortBackchannelES))]
	case LanguageFr:
		return shortBackchannelFR[bg.fastRand(len(shortBackchannelFR))]
	case LanguageDe:
		return shortBackchannelDE[bg.fastRand(len(shortBackchannelDE))]
	case LanguagePt:
		return shortBackchannelPT[bg.fastRand(len(shortBackchannelPT))]
	case LanguageIt:
		return shortBackchannelIT[bg.fastRand(len(shortBackchannelIT))]
	default:
		return shortBackchannelEN[bg.fastRand(len(shortBackchannelEN))]
	}
}

func (bg *BackchannelGenerator) fastRand(n int) int {
	return int(math.Abs(float64(time.Now().UnixNano()))) % n
}

var shortBackchannelEN = []string{
	"Mm-hmm.", "Right.", "I see.", "Okay.", "Sure.", "Got it.", "Uh-huh.",
}

var shortBackchannelES = []string{
	"Ajá.", "Claro.", "Entiendo.", "Sí.", "Vale.", "Ya veo.", "Mm-hmm.",
}

var shortBackchannelFR = []string{
	"Mm-hmm.", "D'accord.", "Je vois.", "Oui.", "Bien sûr.", "Compris.",
}

var shortBackchannelDE = []string{
	"Mm-hmm.", "Aha.", "Verstehe.", "Ja.", "Okay.", "Stimmt.",
}

var shortBackchannelPT = []string{
	"Mm-hmm.", "Claro.", "Entendo.", "Sim.", "Tá bem.", "Sei.",
}

var shortBackchannelIT = []string{
	"Mm-hmm.", "Certo.", "Capisco.", "Sì.", "Va bene.", "Ho capito.",
}

func (bg *BackchannelGenerator) SynthesiszeBackchannel(ctx context.Context, text string) ([]byte, error) {
	bg.synthesisLock.Lock()
	defer bg.synthesisLock.Unlock()

	if cached, ok := bg.cachedAudio[text]; ok {
		return cached, nil
	}

	audio, err := bg.orch.Synthesize(ctx, text, bg.session.GetCurrentVoice(), bg.session.GetCurrentLanguage())
	if err != nil {
		return nil, err
	}

	if len(audio) > 0 {
		if bg.cachedAudio == nil {
			bg.cachedAudio = make(map[string][]byte)
		}
		bg.cachedAudio[text] = audio
	}

	return audio, nil
}

func (bg *BackchannelGenerator) PreWarm(ctx context.Context) {
	if !bg.enabled {
		return
	}

	texts := shortBackchannelEN
	switch bg.language {
	case LanguageEs:
		texts = shortBackchannelES
	case LanguageFr:
		texts = shortBackchannelFR
	case LanguageDe:
		texts = shortBackchannelDE
	case LanguagePt:
		texts = shortBackchannelPT
	case LanguageIt:
		texts = shortBackchannelIT
	}

	voice := bg.session.GetCurrentVoice()
	lang := bg.session.GetCurrentLanguage()

	for _, t := range texts {
		audio, err := bg.orch.tts.Synthesize(ctx, t, voice, lang)
		if err != nil {
			continue
		}
		if bg.cachedAudio == nil {
			bg.cachedAudio = make(map[string][]byte)
		}
		bg.cachedAudio[t] = audio
	}
}

type BackchannelMixer struct {
	active []byte
	offset int
}

func (bm *BackchannelMixer) Play(chunk []byte) {
	bm.active = append(bm.active, chunk...)
}

func (bm *BackchannelMixer) Mix(chunk []byte, volume float64) []byte {
	if len(bm.active) == 0 {
		return chunk
	}

	out := make([]byte, len(chunk))
	copy(out, chunk)

	for i := 0; i < len(chunk) && bm.offset < len(bm.active); i += 2 {
		if i+1 >= len(chunk) || bm.offset+1 >= len(bm.active) {
			break
		}

		inputSample := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		bcSample := int16(bm.active[bm.offset]) | (int16(bm.active[bm.offset+1]) << 8)

		mixed := int(float64(inputSample) + float64(bcSample)*volume)
		if mixed > 32767 {
			mixed = 32767
		} else if mixed < -32768 {
			mixed = -32768
		}

		out[i] = byte(mixed)
		out[i+1] = byte(mixed >> 8)
		bm.offset += 2
	}

	if bm.offset >= len(bm.active) {
		bm.active = nil
		bm.offset = 0
	}

	return out
}

func IsLikelyBackchannelAcoustic(transcript string, segmentRMS float64, baselineRMS float64, segmentDuration time.Duration, threshold float64) bool {
	if baselineRMS <= 0 {
		baselineRMS = 0.02
	}

	energyRatio := segmentRMS / baselineRMS

	if threshold <= 0 {
		threshold = 0.4
	}
	if energyRatio < threshold {
		return true
	}

	words := countWords(transcript)
	if words <= 2 && segmentDuration < 400*time.Millisecond {
		return true
	}

	if words <= 1 && energyRatio < 0.7 {
		return true
	}

	return false
}

func (bg *BackchannelGenerator) GetBaselineRMS() float64 {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	return bg.userBaselineRMS
}
