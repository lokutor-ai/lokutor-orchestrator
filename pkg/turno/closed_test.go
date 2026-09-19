package turno

import "testing"

// A Step after Destroy must return an error, never panic.
//
// Destroy frees the ONNX tensors. Both it and Step take the same mutex, so there is no data race —
// but nothing ordered them, and a Step that acquired the lock afterwards read a freed tensor:
//
//	panic: slice bounds out of range [:46] with capacity 0
//
// Recovered upstream, which was worse than crashing: it aborted handleAudio after the VAD had
// computed its event but before that event was acted on, so the turn never closed and the caller
// waited for a reply that could not come. Seen in production as "I said something and it just
// stayed quiet as if I hadn't finished talking".
func TestStepAfterDestroyReturnsErrorInsteadOfPanicking(t *testing.T) {
	r := &Runtime{}
	r.Destroy() // no tensors were ever allocated; Destroy must tolerate that too

	near := make([]float32, Hop)
	far := make([]float32, Hop)

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Step panicked on a destroyed runtime: %v", p)
		}
	}()

	if _, err := r.Step(near, far); err == nil {
		t.Error("Step on a destroyed runtime returned no error; callers rely on the error to skip the frame")
	}
}

// Destroy must be safe to call more than once — Close paths are not always reached exactly once.
func TestDestroyIsIdempotent(t *testing.T) {
	r := &Runtime{}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("second Destroy panicked: %v", p)
		}
	}()
	r.Destroy()
	r.Destroy()
}
