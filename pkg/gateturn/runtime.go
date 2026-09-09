package gateturn

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	GRUHidden    = 48
	DuplexHidden = 16
	CtxFrames    = 8 // must match export.py's ctx_frames
	InDim        = FeatureDim * 2

	// GateHardThreshold and MaxSkipRun mirror runtime.py's stage-1 skip
	// approximation: once the trained gate g_t has stayed low for this many
	// consecutive frames, skip the graph entirely and reuse the last
	// decision, rather than paying for a session.Run on every frame.
	GateHardThreshold = 0.05
	MaxSkipRun        = 8

	energyDeltaThresh = 0.0015
	maxQuietRun       = 15
)

// Decision mirrors runtime.py's TurnDecision: the model's output for one
// frame, plus whether this frame actually ran the graph (false means the
// compute cascade reused the previous decision).
type Decision struct {
	VAD            float32
	TurnState      [4]float32 // p_complete, p_incomplete, p_backchannel, p_wait
	Bargein        float32    // 0-1, "is this near/far overlap a real interruption"
	Horizon        [3]float32 // p_end_within_{200,500,800}ms
	Stage1Computed bool
}

// TurnStateLabel returns the argmax label, matching Python's turn_state_label.
func (d Decision) TurnStateLabel() string {
	labels := [4]string{"complete", "incomplete", "backchannel", "wait"}
	best := 0
	for i := 1; i < 4; i++ {
		if d.TurnState[i] > d.TurnState[best] {
			best = i
		}
	}
	return labels[best]
}

// ShouldYieldFloor is a convenience for a TTS-playback loop: is bargein
// confident enough to be a real interruption, not a backchannel?
func (d Decision) ShouldYieldFloor(threshold float32) bool {
	return d.Bargein >= threshold
}

// Runtime is the real-time, frame-by-frame GateTurn inference wrapper —
// a Go port of turn-taking/src/runtime.py's GateTurnRuntime, using
// onnxruntime_go instead of onnxruntime's Python bindings.
//
// far_frame is the agent's own outgoing TTS audio (loopback) for the same
// 20ms window as near_frame, or all-zeros if the agent isn't speaking or
// this is a plain VAD use case. Call Reset() between separate calls/
// sessions to clear internal state.
type Runtime struct {
	mu sync.Mutex

	session *ort.AdvancedSession

	featWindow  *ort.Tensor[float32]
	energyDelta *ort.Tensor[float32]
	hPrev       *ort.Tensor[float32]
	dPrev       *ort.Tensor[float32]
	diffPrev    *ort.Tensor[float32]

	vadOut       *ort.Tensor[float32]
	turnStateOut *ort.Tensor[float32]
	bargeinOut   *ort.Tensor[float32]
	horizonOut   *ort.Tensor[float32]
	hNewOut      *ort.Tensor[float32]
	gateOut      *ort.Tensor[float32]
	dNewOut      *ort.Tensor[float32]
	diffNewOut   *ort.Tensor[float32]

	fxNear *CausalFeatureExtractor
	fxFar  *CausalFeatureExtractor

	featRing    [CtxFrames][InDim]float32 // rolling window fed to the graph
	prevEnergy  float32
	haveEnergy  bool
	skipRun     int
	last        Decision
	initialized bool

	Stats struct {
		Frames         int
		Stage0Computed int
		Stage1Computed int
	}
}

// NewRuntime loads the GateTurn ONNX model at modelPath.
func NewRuntime(modelPath string) (*Runtime, error) {
	if !ort.IsInitialized() {
		libPath := os.Getenv("ONNXRUNTIME_LIB_PATH")
		if libPath == "" {
			if runtime.GOOS == "darwin" {
				libPath = "/opt/homebrew/lib/libonnxruntime.dylib"
			} else {
				libPath = "/usr/local/lib/libonnxruntime.so"
			}
		}
		ort.SetSharedLibraryPath(libPath)
		if err := ort.InitializeEnvironment(); err != nil {
			if !strings.Contains(err.Error(), "already been initialized") {
				return nil, fmt.Errorf("init onnx: %w", err)
			}
		}
	}

	r := &Runtime{
		fxNear: NewCausalFeatureExtractor(),
		fxFar:  NewCausalFeatureExtractor(),
	}

	var err error
	if r.featWindow, err = ort.NewEmptyTensor[float32]([]int64{1, CtxFrames, InDim}); err != nil {
		return nil, fmt.Errorf("feat_window tensor: %w", err)
	}
	if r.energyDelta, err = ort.NewEmptyTensor[float32]([]int64{1, 1}); err != nil {
		return nil, fmt.Errorf("energy_delta tensor: %w", err)
	}
	if r.hPrev, err = ort.NewEmptyTensor[float32]([]int64{1, GRUHidden}); err != nil {
		return nil, fmt.Errorf("h_prev tensor: %w", err)
	}
	if r.dPrev, err = ort.NewEmptyTensor[float32]([]int64{1, DuplexHidden}); err != nil {
		return nil, fmt.Errorf("d_prev tensor: %w", err)
	}
	if r.diffPrev, err = ort.NewEmptyTensor[float32]([]int64{1}); err != nil {
		return nil, fmt.Errorf("diff_prev tensor: %w", err)
	}

	if r.vadOut, err = ort.NewEmptyTensor[float32]([]int64{1, 1}); err != nil {
		return nil, fmt.Errorf("vad output tensor: %w", err)
	}
	if r.turnStateOut, err = ort.NewEmptyTensor[float32]([]int64{1, 4}); err != nil {
		return nil, fmt.Errorf("turn_state output tensor: %w", err)
	}
	if r.bargeinOut, err = ort.NewEmptyTensor[float32]([]int64{1, 1}); err != nil {
		return nil, fmt.Errorf("bargein output tensor: %w", err)
	}
	if r.horizonOut, err = ort.NewEmptyTensor[float32]([]int64{1, 3}); err != nil {
		return nil, fmt.Errorf("horizon output tensor: %w", err)
	}
	if r.hNewOut, err = ort.NewEmptyTensor[float32]([]int64{1, GRUHidden}); err != nil {
		return nil, fmt.Errorf("h_new output tensor: %w", err)
	}
	if r.gateOut, err = ort.NewEmptyTensor[float32]([]int64{1, 1}); err != nil {
		return nil, fmt.Errorf("gate output tensor: %w", err)
	}
	if r.dNewOut, err = ort.NewEmptyTensor[float32]([]int64{1, DuplexHidden}); err != nil {
		return nil, fmt.Errorf("d_new output tensor: %w", err)
	}
	if r.diffNewOut, err = ort.NewEmptyTensor[float32]([]int64{1}); err != nil {
		return nil, fmt.Errorf("diff_new output tensor: %w", err)
	}

	r.session, err = ort.NewAdvancedSession(
		modelPath,
		[]string{"feat_window", "energy_delta", "h_prev", "d_prev", "diff_prev"},
		[]string{"vad", "turn_state", "bargein", "horizon", "h_new", "gate", "d_new", "diff_new"},
		[]ort.ArbitraryTensor{r.featWindow, r.energyDelta, r.hPrev, r.dPrev, r.diffPrev},
		[]ort.ArbitraryTensor{r.vadOut, r.turnStateOut, r.bargeinOut, r.horizonOut, r.hNewOut, r.gateOut, r.dNewOut, r.diffNewOut},
		nil,
	)
	if err != nil {
		r.destroyTensors()
		return nil, fmt.Errorf("load onnx model: %w", err)
	}

	r.last = Decision{TurnState: [4]float32{0.25, 0.25, 0.25, 0.25}}
	return r, nil
}

// Reset clears all internal recurrent/streaming state, matching Python's
// GateTurnRuntime.reset(). Call between separate calls/sessions.
func (r *Runtime) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fxNear.Reset()
	r.fxFar.Reset()
	for i := range r.featRing {
		for j := range r.featRing[i] {
			r.featRing[i][j] = 0
		}
	}
	for i := range r.hPrev.GetData() {
		r.hPrev.GetData()[i] = 0
	}
	for i := range r.dPrev.GetData() {
		r.dPrev.GetData()[i] = 0
	}
	r.diffPrev.GetData()[0] = 0
	r.haveEnergy = false
	r.skipRun = 0
	r.last = Decision{TurnState: [4]float32{0.25, 0.25, 0.25, 0.25}}
	r.Stats.Frames, r.Stats.Stage0Computed, r.Stats.Stage1Computed = 0, 0, 0
}

// Step runs one 20ms frame (exactly Hop=320 samples each, float32 @16kHz,
// already normalized to roughly [-1, 1]) through the compute cascade and
// returns the current turn-taking decision. Pass a nil or all-zero farFrame
// when there is no far-end (agent TTS) audio for this window.
func (r *Runtime) Step(nearFrame, farFrame []float32) (Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.Stats.Frames++
	if farFrame == nil {
		farFrame = make([]float32, Hop)
	}

	nearFeat, computed0 := r.fxNear.PushFrameCascaded(nearFrame, energyDeltaThresh, maxQuietRun)
	farFeat, _ := r.fxFar.PushFrameCascaded(farFrame, energyDeltaThresh, maxQuietRun)
	if computed0 {
		r.Stats.Stage0Computed++
	}

	var feat [InDim]float32
	copy(feat[:FeatureDim], nearFeat)
	copy(feat[FeatureDim:], farFeat)

	energy := feat[EnergyIdx]
	var energyDelta float32
	if r.haveEnergy {
		energyDelta = energy - r.prevEnergy
		if energyDelta < 0 {
			energyDelta = -energyDelta
		}
	} else {
		energyDelta = 0
	}
	r.prevEnergy = energy
	r.haveEnergy = true

	// Roll the context window: drop the oldest frame, append this one —
	// matches Python's np.roll(feat_ring, -1, axis=0).
	copy(r.featRing[0:CtxFrames-1], r.featRing[1:CtxFrames])
	r.featRing[CtxFrames-1] = feat

	// Stage-1 skip: the trained gate g_t is itself computed inside the ONNX
	// graph, so there's no way to know it without running the graph. This is
	// therefore a lag-1 approximation — if the *previous* frame's gate was
	// low, assume this frame is still in the same steady region and reuse
	// the last decision instead of paying for a session.Run at all. See
	// runtime.py for the identical reasoning.
	if r.skipRun > 0 && r.skipRun < MaxSkipRun {
		r.skipRun++
		out := r.last
		out.Stage1Computed = false
		return out, nil
	}

	windowData := r.featWindow.GetData()
	for i := 0; i < CtxFrames; i++ {
		copy(windowData[i*InDim:(i+1)*InDim], r.featRing[i][:])
	}
	r.energyDelta.GetData()[0] = energyDelta

	if err := r.session.Run(); err != nil {
		return Decision{}, fmt.Errorf("gateturn inference: %w", err)
	}

	g := r.gateOut.GetData()[0]
	fire := g > GateHardThreshold || r.skipRun >= MaxSkipRun
	if !fire {
		r.skipRun++
		out := r.last
		out.Stage1Computed = false
		return out, nil
	}

	copy(r.hPrev.GetData(), r.hNewOut.GetData())
	copy(r.dPrev.GetData(), r.dNewOut.GetData())
	r.diffPrev.GetData()[0] = r.diffNewOut.GetData()[0]
	r.skipRun = 0
	r.Stats.Stage1Computed++

	ts := r.turnStateOut.GetData()
	hz := r.horizonOut.GetData()
	r.last = Decision{
		VAD:            r.vadOut.GetData()[0],
		TurnState:      [4]float32{ts[0], ts[1], ts[2], ts[3]},
		Bargein:        r.bargeinOut.GetData()[0],
		Horizon:        [3]float32{hz[0], hz[1], hz[2]},
		Stage1Computed: true,
	}
	return r.last, nil
}

func (r *Runtime) destroyTensors() {
	// Explicit nil checks per field, not a []interface{ Destroy() } slice —
	// a nil *ort.Tensor[float32] boxed into that interface is a non-nil
	// interface value (the classic Go typed-nil gotcha), so a slice-based
	// `if t != nil` guard would still call Destroy() on a nil pointer for
	// any tensor that was never allocated (e.g. after a partial failure in
	// NewRuntime).
	if r.featWindow != nil {
		r.featWindow.Destroy()
	}
	if r.energyDelta != nil {
		r.energyDelta.Destroy()
	}
	if r.hPrev != nil {
		r.hPrev.Destroy()
	}
	if r.dPrev != nil {
		r.dPrev.Destroy()
	}
	if r.diffPrev != nil {
		r.diffPrev.Destroy()
	}
	if r.vadOut != nil {
		r.vadOut.Destroy()
	}
	if r.turnStateOut != nil {
		r.turnStateOut.Destroy()
	}
	if r.bargeinOut != nil {
		r.bargeinOut.Destroy()
	}
	if r.horizonOut != nil {
		r.horizonOut.Destroy()
	}
	if r.hNewOut != nil {
		r.hNewOut.Destroy()
	}
	if r.gateOut != nil {
		r.gateOut.Destroy()
	}
	if r.dNewOut != nil {
		r.dNewOut.Destroy()
	}
	if r.diffNewOut != nil {
		r.diffNewOut.Destroy()
	}
}

// Destroy releases the ONNX session and all tensors. Safe to call once.
func (r *Runtime) Destroy() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != nil {
		r.session.Destroy()
		r.session = nil
	}
	r.destroyTensors()
}
