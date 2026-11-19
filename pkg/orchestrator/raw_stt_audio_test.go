package orchestrator

import (
	"bytes"
	"context"
	"testing"
)

// recVAD records every chunk the VAD is given and reports speech while speaking is set.
type recVAD struct {
	speaking bool
	seen     [][]byte
}

func (v *recVAD) Process(chunk []byte) (*VADEvent, error) {
	v.seen = append(v.seen, append([]byte(nil), chunk...))
	return nil, nil
}
func (v *recVAD) IsSpeaking() bool   { return v.speaking }
func (v *recVAD) Reset()             {}
func (v *recVAD) Clone() VADProvider { return v }
func (v *recVAD) Name() string       { return "rec" }

// 2026-11-19 (HireVoicer phone call, after the adaptive echo floor): a quiet caller's interruptions registered, but
// the recogniser was fed what the echo gate left of their voice (soft phonemes and onsets zeroed) and came back with
// "De Yes.", "Yeo.", "El al.". With WriteDual the VAD judges the gated audio and the speech buffers keep the raw.
func TestWriteDualVADSeesGatedSTTGetsRaw(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FirstSpeaker = FirstSpeakerUser
	cfg.SampleRate = 16000
	cfg.SilenceTimeout = 0
	vad := &recVAD{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hola"}, &MockLLMProvider{}, &MockTTSProvider{}, vad, cfg, &recordingLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("dual"))
	defer ms.Close()

	var raws [][]byte
	// 300 ms before onset (the lead-in), then 600 ms of speech whose soft frames the gate zeroed.
	for i := 0; i < 45; i++ {
		raw := toneAt(-31, 20)
		gated := raw
		if i%3 == 1 { // every third frame under the gate's floor: zeroed for the VAD only
			gated = make([]byte, len(raw))
		}
		vad.speaking = i >= 15
		raws = append(raws, raw)
		ms.handleAudioIn(raw, gated)
		if !bytes.Equal(vad.seen[len(vad.seen)-1], gated) {
			t.Fatalf("frame %d: the VAD must judge the gated audio", i)
		}
	}
	want := bytes.Join(raws[15:], nil)
	ms.mu.Lock()
	got := append([]byte(nil), ms.speechAudioBuf...)
	pre := append([]byte(nil), ms.preSpeechBuf.Bytes()...)
	ms.mu.Unlock()
	if !bytes.Equal(got, want) {
		t.Fatalf("speech buffer: %d bytes, want the caller's raw %d bytes (zeroed frames must not reach the recogniser)", len(got), len(want))
	}
	if !bytes.Equal(pre, bytes.Join(raws[len(raws)-len(pre)/640:], nil)) {
		t.Fatal("the lead-in buffer must hold the raw audio")
	}
}

// Write (one stream) is unchanged: VAD and recogniser get the same audio.
func TestWriteSingleStreamUnchanged(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FirstSpeaker = FirstSpeakerUser
	cfg.SampleRate = 16000
	cfg.SilenceTimeout = 0
	vad := &recVAD{speaking: true}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hola"}, &MockLLMProvider{}, &MockTTSProvider{}, vad, cfg, &recordingLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("single"))
	defer ms.Close()
	c := toneAt(-31, 20)
	ms.handleAudio(c)
	if !bytes.Equal(vad.seen[0], c) {
		t.Fatal("single-stream audio must reach the VAD unchanged")
	}
}
