package orchestrator

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This repository is public and model weights are private (the Turno model was committed here from 10 September
// to 7 October 2026 and had to be removed). A weights file must never be tracked: the host provides them at run
// time, and tests that need one read TURNO_TEST_MODEL. Files that are merely present on a developer's disk are
// fine (they are git-ignored); this looks at what git would publish.
func TestNoModelWeightsAreTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--", ":/").Output() // ":/": the whole repository, not just this package's directory
	if err != nil {
		t.Skipf("not a git checkout, or git is unavailable: %v", err)
	}
	weights := map[string]bool{".onnx": true, ".pt": true, ".pth": true, ".safetensors": true, ".gguf": true, ".tflite": true, ".ckpt": true, ".npz": true}
	var found []string
	for _, f := range strings.Split(string(out), "\x00") {
		ext := strings.ToLower(filepath.Ext(f))
		if weights[ext] || strings.HasSuffix(strings.ToLower(f), ".onnx.data") {
			found = append(found, f)
		}
	}
	if len(found) > 0 {
		t.Fatalf("model weights are tracked in this public repository, remove them (git rm --cached): %v", found)
	}
}
