package gateturn

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// fixture mirrors the golden fixture generated from the Python reference
// implementation (turn-taking/src/features.py) via:
//
//	python3 -c "..." > /tmp/gateturn_fixture.json
//
// See the command in the PR/commit description. This test exists to catch
// any numerical drift between this Go port and the Python original, since a
// silent mismatch here would degrade the model's real accuracy in a way
// that's invisible without this kind of golden-value check.
type fixture struct {
	FramesIn  [][]float32 `json:"frames_in"`
	FramesOut [][]float32 `json:"frames_out"`
}

func TestPushFrameMatchesPythonReference(t *testing.T) {
	data, err := os.ReadFile("/tmp/gateturn_fixture.json")
	if err != nil {
		t.Skipf("golden fixture not present (%v) — regenerate with the Python reference before trusting this port", err)
	}
	var fx fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	extractor := NewCausalFeatureExtractor()
	for i, in := range fx.FramesIn {
		got := extractor.PushFrame(in)
		want := fx.FramesOut[i]
		if len(got) != len(want) {
			t.Fatalf("frame %d: dim mismatch got=%d want=%d", i, len(got), len(want))
		}
		for j := range want {
			diff := math.Abs(float64(got[j] - want[j]))
			// mel-log/energy-db values range roughly -6..2 and pitch confidence
			// 0..1 — 1e-3 is well below anything that would matter to the model
			// but tight enough to catch a real algorithmic mismatch.
			if diff > 1e-3 {
				t.Errorf("frame %d, feat %d: got %v want %v (diff %v)", i, j, got[j], want[j], diff)
			}
		}
	}
}
