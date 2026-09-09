package gateturn

import (
	"math"
	"testing"
)

const testModelPath = "../../assets/onnx/gateturn/model.onnx"

func TestRuntimeStepProducesSaneDecisions(t *testing.T) {
	rt, err := NewRuntime(testModelPath)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Destroy()

	silence := make([]float32, Hop)
	loud := make([]float32, Hop)
	for i := range loud {
		loud[i] = float32(0.4 * math.Sin(2*math.Pi*220*float64(i)/float64(SampleRate)))
	}

	var last Decision
	// Feed enough silence, then enough loud "speech" frames, to get past the
	// stage-1 skip cascade and observe a real forward pass on both regimes.
	for i := 0; i < 40; i++ {
		last, err = rt.Step(silence, nil)
		if err != nil {
			t.Fatalf("Step (silence, frame %d): %v", i, err)
		}
	}
	for _, v := range last.TurnState {
		if v < 0 || v > 1 {
			t.Errorf("turn_state out of [0,1]: %v", last.TurnState)
		}
	}
	if last.VAD < 0 || last.VAD > 1 {
		t.Errorf("vad out of [0,1]: %v", last.VAD)
	}

	for i := 0; i < 60; i++ {
		last, err = rt.Step(loud, loud)
		if err != nil {
			t.Fatalf("Step (loud, frame %d): %v", i, err)
		}
	}
	if last.Bargein < 0 || last.Bargein > 1 {
		t.Errorf("bargein out of [0,1]: %v", last.Bargein)
	}

	t.Logf("frames=%d stage0Computed=%d stage1Computed=%d finalVAD=%.3f finalBargein=%.3f label=%s",
		rt.Stats.Frames, rt.Stats.Stage0Computed, rt.Stats.Stage1Computed, last.VAD, last.Bargein, last.TurnStateLabel())

	if rt.Stats.Stage0Computed >= rt.Stats.Frames {
		t.Errorf("expected the DSP-level cascade to skip at least some frames on sustained silence/tone, got stage0Computed=%d frames=%d", rt.Stats.Stage0Computed, rt.Stats.Frames)
	}
}

func TestRuntimeResetClearsState(t *testing.T) {
	rt, err := NewRuntime(testModelPath)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Destroy()

	loud := make([]float32, Hop)
	for i := range loud {
		loud[i] = float32(0.4 * math.Sin(2*math.Pi*220*float64(i)/float64(SampleRate)))
	}
	for i := 0; i < 20; i++ {
		if _, err := rt.Step(loud, loud); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	rt.Reset()
	if rt.Stats.Frames != 0 {
		t.Errorf("Reset did not clear Stats.Frames: %d", rt.Stats.Frames)
	}
}
