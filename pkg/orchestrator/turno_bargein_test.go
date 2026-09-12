package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/turno"
)

// turnoTestModelPath must match where the integration copied
// turn-taking/checkpoints_v3_aec/gateturn.onnx for production use.
const turnoTestModelPath = "../../assets/onnx/turno/model.onnx"

// requireTurnoModel skips (not fails) unless a Turno runtime can
// actually be constructed here — covers both the model file being absent
// and, just as importantly, the ONNX runtime shared library itself not
// being installed in this environment (e.g. a CI runner with no
// libonnxruntime.so — a real environment gap hit in CI, not a code
// regression). An os.Stat-only check missed that second case entirely.
func requireTurnoModel(t *testing.T) {
	t.Helper()
	rt, err := turno.NewRuntime(turnoTestModelPath)
	if err != nil {
		t.Skipf("Turno runtime unavailable: %v", err)
	}
	rt.Destroy()
}

// loudFrame16k returns synthetic 16kHz PCM16 audio loud enough to register
// as speech on both a plain RMS VAD and Turno's own energy features.
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

// TestTurnoBargein_AssistTracksPeakScore feeds sustained near+far energy
// (an "overlap" that never recedes) while a tentative barge-in is open, and
// checks the peak bargein score is tracked and crosses the assist threshold
// — but that pendingBargeIn is NOT cleared on its own. Turno no longer
// confirms a barge-in unilaterally (see turno_bargein.go); it only
// informs processUtterance's MinWordsToInterrupt gate once STT reaches it.
func TestTurnoBargein_AssistTracksPeakScore(t *testing.T) {
	requireTurnoModel(t)

	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.TurnoModelPath = turnoTestModelPath
	cfg.TurnoBargeinAssistThreshold = 0.5
	cfg.TurnoBargeinAssistWordsRelief = 1

	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.turno == nil {
		t.Fatal("Turno runtime did not load — check assets/onnx/turno/model.onnx")
	}

	// Simulate the bot mid-speech with a tentative barge-in already open,
	// as onVADStart would set up on a real raw-VAD trigger.
	stream.mu.Lock()
	stream.state = StateListening
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.turnoBargeinPeakScore = 0
	stream.mu.Unlock()

	near := loudFrame16k(320)
	far := loudFrame16k(320)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.noteFarEndAudio(far)
		stream.feedTurno(near)

		stream.mu.Lock()
		peak := stream.turnoBargeinPeakScore
		stillPending := stream.pendingBargeIn
		stream.mu.Unlock()

		if !stillPending {
			t.Fatal("Turno must never clear pendingBargeIn on its own — that decision belongs to STT-based checks in processUtterance")
		}
		if peak >= cfg.TurnoBargeinAssistThreshold {
			return // peak score reached the assist threshold — wiring works end to end
		}
	}
	t.Fatal("Turno bargein peak score never reached the assist threshold on sustained loud near+far audio")
}

// TestTurnoBargein_Disabled verifies the feature is a true no-op when
// TurnoModelPath is unset — the existing STT-confirmation path is the
// only thing that can act on pendingBargeIn.
func TestTurnoBargein_Disabled(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig() // TurnoModelPath left empty
	cfg.SilenceTimeout = 0
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.turno != nil {
		t.Fatal("expected Turno runtime to be nil when TurnoModelPath is unset")
	}

	stream.mu.Lock()
	stream.state = StateListening
	stream.pendingBargeIn = true
	stream.pendingBargeGen = stream.payloadGen
	stream.mu.Unlock()

	// Should not panic and should not touch pendingBargeIn or peak score.
	stream.feedTurno(loudFrame16k(320))

	stream.mu.Lock()
	pending := stream.pendingBargeIn
	peak := stream.turnoBargeinPeakScore
	stream.mu.Unlock()
	if !pending {
		t.Fatal("feedTurno must be a no-op when Turno isn't loaded")
	}
	if peak != 0 {
		t.Fatal("feedTurno must not update peak score when Turno isn't loaded")
	}
}

// TestTurnoBargein_RunsWhenNotPending verifies Turno now runs
// continuously (for the VAD shadow comparison) even with no pending
// barge-in — unlike the old bargein-only design, audio must not just be
// discarded, and the peak-score/bargein-window state must stay untouched.
func TestTurnoBargein_RunsWhenNotPending(t *testing.T) {
	requireTurnoModel(t)

	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	cfg := DefaultConfig()
	cfg.TurnoModelPath = turnoTestModelPath
	orch := NewWithVAD(stt, llm, tts, vad, cfg)
	session := NewConversationSession("test")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	if stream.turno == nil {
		t.Fatal("Turno runtime did not load")
	}

	// No pendingBargeIn: feeding audio must not crash, must not touch
	// pendingBargeIn/peak score, but should still consume frames (shadow
	// VAD keeps running regardless of barge-in state).
	stream.feedTurno(silentFrame16k(100))
	stream.feedTurno(loudFrame16k(320))

	stream.mu.Lock()
	pending := stream.pendingBargeIn
	peak := stream.turnoBargeinPeakScore
	frames := stream.turnoVadDiagFrames
	stream.mu.Unlock()

	if pending {
		t.Fatal("feedTurno must not open a barge-in on its own")
	}
	if peak != 0 {
		t.Fatal("peak bargein score must stay 0 with no pending barge-in window")
	}
	if frames == 0 {
		t.Error("expected feedTurno to have processed at least one full frame for the VAD shadow comparison")
	}
}
