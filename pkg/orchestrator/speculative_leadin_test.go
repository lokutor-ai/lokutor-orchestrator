package orchestrator

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// recordingSTT captures the audio each Transcribe call was given.
type recordingSTT struct {
	mu    sync.Mutex
	calls [][]byte
	text  string
}

func (r *recordingSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]byte(nil), audio...))
	r.mu.Unlock()
	return TranscriptionResult{Text: r.text}, nil
}
func (r *recordingSTT) Name() string { return "RecordingSTT" }

func (r *recordingSTT) waitCall(t *testing.T) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.calls) > 0 {
			c := r.calls[0]
			r.mu.Unlock()
			return c
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// silentVAD satisfies silenceFramesProvider: the hangover has begun.
type silentVAD struct{ RMSVAD }

func (*silentVAD) SilenceFrames() int { return 5 }

func newLeadInStream(stt STTProvider, log Logger) *ManagedStream {
	cfg := DefaultConfig()
	orch := NewWithLogger(stt, &MockLLMProvider{}, &MockTTSProvider{}, nil, cfg, log)
	return &ManagedStream{
		orch:            orch,
		vad:             &silentVAD{},
		session:         NewConversationSession("test"),
		logger:          log,
		inputSampleRate: 16000,
		speculator:      NewSpeculativeExecutor(300),
		ctx:             context.Background(),
	}
}

// The speculative transcription answers almost every production turn, and it transcribed the
// utterance WITHOUT the ~300ms lead-in the batch path has. On the deploy smoke stimulus that turned
// "Hola, ¿a qué hora abre la oficina mañana?" into "Yeah." + "Abre la oficina mañana." in 68 of 68
// sessions. The snapshot must be lead-in + speech, the same audio userAudio holds.
func TestSpeculativeSnapshotCarriesTheLeadIn(t *testing.T) {
	stt := &recordingSTT{text: "hola"}
	ms := newLeadInStream(stt, &NoOpLogger{})
	leadIn := bytes.Repeat([]byte{1}, 300*32) // 300ms at 16kHz PCM16
	speech := bytes.Repeat([]byte{2}, 400*32) // 400ms of speech
	ms.speechLeadIn = leadIn
	ms.speechAudioBuf = append([]byte(nil), speech...)

	ms.maybeSpeculateSTT()

	got := stt.waitCall(t)
	want := append(append([]byte(nil), leadIn...), speech...)
	if !bytes.Equal(got, want) {
		t.Fatalf("speculative STT got %d bytes (starts %v), want lead-in + speech = %d bytes",
			len(got), got[:1], len(want))
	}

	// And the tail check now compares like with like: userAudio (lead-in + speech + hangover)
	// against a snapshot that also has the lead-in, so only the hangover counts as tail.
	final := len(want) + 480*32
	if _, ok, _ := ms.specSTT.awaitUsable(context.Background(), 1, final, 32, defaultSpecSTTMaxTailMs); !ok {
		t.Error("a snapshot followed only by a 480ms hangover must be usable — the lead-in is not tail")
	}
}

// The minimum-length gate is about how much the caller SAID. Counting the lead-in would let any
// 20ms blip clear a 100ms minimum.
func TestSpeculativeMinLengthIgnoresTheLeadIn(t *testing.T) {
	stt := &recordingSTT{text: "x"}
	ms := newLeadInStream(stt, &NoOpLogger{})
	ms.speechLeadIn = bytes.Repeat([]byte{1}, 300*32)
	ms.speechAudioBuf = bytes.Repeat([]byte{2}, 20*32) // a 20ms blip

	ms.maybeSpeculateSTT()
	time.Sleep(100 * time.Millisecond)

	stt.mu.Lock()
	n := len(stt.calls)
	stt.mu.Unlock()
	if n != 0 {
		t.Errorf("speculated on a 20ms blip because the lead-in counted toward the minimum")
	}
}

// onVADStart hands the same lead-in to both buffers' consumers, and onVADEnd retires it with the
// utterance, so a cooldown-ignored start can never reuse the previous utterance's.
func TestLeadInFollowsTheUtterance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithVAD(&MockSTTProvider{transcribeResult: "hola"}, &MockLLMProvider{},
		&MockTTSProvider{}, NewRMSVAD(0.1, 100*time.Millisecond), cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("test"))
	defer ms.Close()

	pre := bytes.Repeat([]byte{7}, 300*32)
	ms.mu.Lock()
	ms.preSpeechBuf.Reset()
	ms.preSpeechBuf.Write(pre)
	ms.mu.Unlock()

	ms.onVADStart(StateIdle)

	ms.mu.Lock()
	leadIn, user := ms.speechLeadIn, ms.userAudio
	ms.mu.Unlock()
	if !bytes.Equal(leadIn, pre) {
		t.Fatalf("speechLeadIn = %d bytes, want the 300ms pre-roll", len(leadIn))
	}
	if !bytes.HasPrefix(user, pre) {
		t.Fatalf("userAudio lost its pre-roll")
	}
	// userAudio must not alias the lead-in: appending speech to one must not write into the other.
	ms.mu.Lock()
	ms.userAudio = append(ms.userAudio, 9)
	ms.mu.Unlock()
	if len(ms.speechLeadIn) != len(pre) || ms.speechLeadIn[len(pre)-1] != 7 {
		t.Fatal("userAudio and speechLeadIn share a backing array")
	}
}
