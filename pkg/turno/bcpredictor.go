package turno

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// The backchannel-opportunity predictor (lokutor_tts tools/backchannel_predictor): from the caller's audio alone,
// 20 ms at a time, the probability that a human listener would start a backchannel within the next 300 ms. Its
// features are Turno's 23 (PushFrame) plus the log pitch (BCFeatureDim = 24), the model a small GRU stepped one frame
// at a time: feat [1,1,24], h [1,1,64] -> prob [1,2] (lexical, any), h_out [1,1,64].

const (
	BCFeatureDim = FeatureDim + 1
	BCHidden     = 64
	bcVoicedConf = 0.45
)

// autocorrPitch is autocorrPitchConfidence's search returning also the best lag: (confidence, lag in samples).
func autocorrPitch(frame []float32, fmin, fmax float64) (float32, int) {
	n := len(frame)
	var mean float32
	for _, s := range frame {
		mean += s
	}
	mean /= float32(n)
	centered := make([]float64, n)
	var energy float64
	for i, s := range frame {
		v := float64(s - mean)
		centered[i] = v
		energy += v * v
	}
	if energy < 1e-8 {
		return 0, 0
	}
	lagMin, lagMax := int(SampleRate/fmax), int(SampleRate/fmin)
	if lagMax > n-1 {
		lagMax = n - 1
	}
	peak, best := math.Inf(-1), 0
	for lag := lagMin; lag < lagMax; lag++ {
		var sum float64
		for i := 0; i+lag < n; i++ {
			sum += centered[i] * centered[i+lag]
		}
		if sum > peak {
			peak, best = sum, lag
		}
	}
	conf := peak / (energy + 1e-8)
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return float32(conf), best
}

// PushFrameBC is PushFrame plus the log pitch (log2 of F0 / 100 Hz where the frame is voiced, else 0): the
// backchannel predictor's 24 features (tools/backchannel_predictor/features.py).
func (c *CausalFeatureExtractor) PushFrameBC(samples []float32) []float32 {
	f := c.PushFrame(samples)
	conf, lag := autocorrPitch(c.buf, 70.0, 400.0)
	var lp float32
	if conf >= bcVoicedConf && lag > 0 {
		lp = float32(math.Log2(float64(SampleRate) / float64(lag) / 100.0))
	}
	return append(f, lp)
}

// BCPredictor runs the predictor on one caller's audio.
type BCPredictor struct {
	mu      sync.Mutex
	closed  bool
	session *ort.AdvancedSession
	feat    *ort.Tensor[float32]
	h       *ort.Tensor[float32]
	prob    *ort.Tensor[float32]
	hOut    *ort.Tensor[float32]
	fx      *CausalFeatureExtractor
}

func initORT() error {
	if ort.IsInitialized() {
		return nil
	}
	libPath := os.Getenv("ONNXRUNTIME_LIB_PATH")
	if libPath == "" {
		if runtime.GOOS == "darwin" {
			libPath = "/opt/homebrew/lib/libonnxruntime.dylib"
		} else {
			libPath = "/usr/local/lib/libonnxruntime.so"
		}
	}
	ort.SetSharedLibraryPath(libPath)
	if err := ort.InitializeEnvironment(); err != nil && !strings.Contains(err.Error(), "already been initialized") {
		return fmt.Errorf("init onnx: %w", err)
	}
	return nil
}

// NewBCPredictor loads the model at path (one thread, no spinning, as Turno).
func NewBCPredictor(path string) (*BCPredictor, error) {
	if err := initORT(); err != nil {
		return nil, err
	}
	p := &BCPredictor{fx: NewCausalFeatureExtractor()}
	var err error
	if p.feat, err = ort.NewEmptyTensor[float32]([]int64{1, 1, BCFeatureDim}); err != nil {
		return nil, err
	}
	if p.h, err = ort.NewEmptyTensor[float32]([]int64{1, 1, BCHidden}); err != nil {
		p.destroy()
		return nil, err
	}
	if p.prob, err = ort.NewEmptyTensor[float32]([]int64{1, 2}); err != nil {
		p.destroy()
		return nil, err
	}
	if p.hOut, err = ort.NewEmptyTensor[float32]([]int64{1, 1, BCHidden}); err != nil {
		p.destroy()
		return nil, err
	}
	opt, optErr := ort.NewSessionOptions()
	if optErr == nil {
		defer opt.Destroy()
		_ = opt.SetIntraOpNumThreads(1)
		_ = opt.SetInterOpNumThreads(1)
		_ = opt.AddSessionConfigEntry("session.intra_op.allow_spinning", "0")
		_ = opt.AddSessionConfigEntry("session.inter_op.allow_spinning", "0")
	} else {
		opt = nil
	}
	p.session, err = ort.NewAdvancedSession(path, []string{"feat", "h"}, []string{"prob", "h_out"},
		[]ort.ArbitraryTensor{p.feat, p.h}, []ort.ArbitraryTensor{p.prob, p.hOut}, opt)
	if err != nil {
		p.destroy()
		return nil, fmt.Errorf("load backchannel predictor: %w", err)
	}
	return p, nil
}

// Step takes exactly Hop samples (16 kHz, [-1, 1]) and returns (p lexical, p any).
func (p *BCPredictor) Step(frame []float32) (float32, float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, 0, errors.New("backchannel predictor closed")
	}
	copy(p.feat.GetData(), p.fx.PushFrameBC(frame))
	if err := p.session.Run(); err != nil {
		return 0, 0, err
	}
	copy(p.h.GetData(), p.hOut.GetData())
	pr := p.prob.GetData()
	return pr[0], pr[1], nil
}

func (p *BCPredictor) destroy() {
	for _, t := range []*ort.Tensor[float32]{p.feat, p.h, p.prob, p.hOut} {
		if t != nil {
			_ = t.Destroy()
		}
	}
}

// Destroy frees the model; Step afterwards returns an error.
func (p *BCPredictor) Destroy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.session != nil {
		_ = p.session.Destroy()
	}
	p.destroy()
}
