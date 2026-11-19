package turno

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The predictor's 24 features in Go match tools/backchannel_predictor/features.py (the training features) on a
// falling-pitch tone after near-silence; the fixture was written by that script.
func TestPushFrameBCMatchesTrainingFeatures(t *testing.T) {
	data, err := os.ReadFile("testdata/bc_features.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	ex := NewCausalFeatureExtractor()
	for i, in := range fx.FramesIn {
		got := ex.PushFrameBC(in)
		if len(got) != BCFeatureDim {
			t.Fatalf("dim %d", len(got))
		}
		for j, w := range fx.FramesOut[i] {
			tol := 2e-3
			if j == BCFeatureDim-1 {
				tol = 0.02 // log pitch: an integer lag, a hair apart in the float sums can pick its neighbour
			}
			if d := math.Abs(float64(got[j] - w)); d > tol {
				t.Errorf("frame %d feature %d: got %v want %v", i, j, got[j], w)
			}
		}
	}
}

// A model at BC_PREDICTOR_TEST_MODEL (an export of train.py) steps and answers two probabilities.
func TestBCPredictorSteps(t *testing.T) {
	path := os.Getenv("BC_PREDICTOR_TEST_MODEL")
	if path == "" {
		t.Skip("BC_PREDICTOR_TEST_MODEL not set")
	}
	p, err := NewBCPredictor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Destroy()
	frame := make([]float32, Hop)
	for i := 0; i < 50; i++ {
		for k := range frame {
			frame[k] = float32(0.3 * math.Sin(2*math.Pi*150*float64(i*Hop+k)/SampleRate))
		}
		a, b, err := p.Step(frame)
		if err != nil || a < 0 || a > 1 || b < 0 || b > 1 {
			t.Fatalf("step %d: %v %v %v", i, a, b, err)
		}
	}
	p.Destroy()
	if _, _, err := p.Step(frame); err == nil {
		t.Fatal("step after destroy did not fail")
	}
}
