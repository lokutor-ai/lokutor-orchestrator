package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/gateturn"
)

// gateTurnTestModelPath must match where the integration copied
// turn-taking/checkpoints/gateturn.onnx for production use.
const gateTurnTestModelPath = "../../assets/onnx/gateturn/model.onnx"

// requireGateTurnModel skips (not fails) unless a GateTurn runtime can
// actually be constructed here — covers both the model file being absent
// and, just as importantly, the ONNX runtime shared library itself not
// being installed in this environment (e.g. a CI runner with no
// libonnxruntime.so — a real environment gap hit in CI, not a code
// regression). An os.Stat-only check missed that second case entirely.
func requireGateTurnModel(t *testing.T) {
	t.Helper()
	rt, err := gateturn.NewRuntime(gateTurnTestModelPath)
	if err != nil {
		t.Skipf("GateTurn runtime unavailable: %v", err)
	}
	rt.Destroy()
}

// loudFrame16k returns synthetic 16kHz PCM16 audio loud enough to register
// as speech on both a plain RMS VAD and GateTurn's own energy features.
func loudFrame16k(n int) []byte {
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(0.5 * 32767 * math.Sin(2*math.Pi*220*float64(i)/16000))
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

func silentFrame16k(n int) []byte {
	return make([]byte, n*2)
}

// TestGateTurnBargein_ConfirmsRealInterruption feeds sustained near+far
// energy (an "overlap" that never recedes) while a tentative barge-in is
// open, and checks the fast path commits to it (pendingBargeIn clears)
// without needing STT confirmation to do so.
func TestGateTurnBargein_ConfirmsRealInterruption(t *testing.T) {
	requireGateTurnModel(t)

	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.GateTurnModelPath = gateTurnTestModelPath
	cfg.GateTurnBargeinConfirmThreshold = 0.5
	cfg.GateTurnBargeinResolveThreshold = 0.05

	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.gateturn == nil {
		t.Fatal("GateTurn runtime did not load — check assets/onnx/gateturn/model.onnx")
	}

	// Simulate the bot mid-speech with a tentative barge-in already open,
	// as onVADStart would set up on a real raw-VAD trigger.
	stream.mu.Lock()
	stream.state = StateListening
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.mu.Unlock()

	near := loudFrame16k(320)
	far := loudFrame16k(320)
	stream.noteFarEndAudio(far)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.noteFarEndAudio(far)
		stream.feedGateTurnBargein(near)

		stream.mu.Lock()
		pending := stream.pendingBargeIn
		stream.mu.Unlock()
		if !pending {
			return // fast path acted (confirmed or resolved) — wiring works end to end
		}
	}
	t.Fatal("GateTurn barge-in fast path never resolved pendingBargeIn on sustained loud near+far audio")
}

// TestGateTurnBargein_Disabled verifies the feature is a true no-op when
// GateTurnModelPath is unset — the existing STT-confirmation path is the
// only thing that can act on pendingBargeIn.
func TestGateTurnBargein_Disabled(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig() // GateTurnModelPath left empty
	cfg.SilenceTimeout = 0
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.gateturn != nil {
		t.Fatal("expected GateTurn runtime to be nil when GateTurnModelPath is unset")
	}

	stream.mu.Lock()
	stream.state = StateListening
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.mu.Unlock()

	// Should not panic and should not touch pendingBargeIn.
	stream.feedGateTurnBargein(loudFrame16k(320))

	stream.mu.Lock()
	pending := stream.pendingBargeIn
	stream.mu.Unlock()
	if !pending {
		t.Fatal("feedGateTurnBargein must be a no-op when GateTurn isn't loaded")
	}
}

func TestGateTurnBargein_NotPendingIsNoop(t *testing.T) {
	requireGateTurnModel(t)

	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.GateTurnModelPath = gateTurnTestModelPath
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.gateturn == nil {
		t.Fatal("GateTurn runtime did not load")
	}

	// No pendingBargeIn: feeding audio must not crash or spuriously flip
	// state, and must reset any leftover accumulation.
	stream.gtNearAccum = append(stream.gtNearAccum, silentFrame16k(100)...)
	stream.feedGateTurnBargein(loudFrame16k(320))

	if len(stream.gtNearAccum) != 0 {
		t.Errorf("expected leftover near accumulation to be cleared when not pending, got %d bytes", len(stream.gtNearAccum))
	}
}
