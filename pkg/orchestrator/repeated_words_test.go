package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Replays the end of phone call telnyx_1794394454647131821 (2026-11-11, Spanish sales agent): the
// caller's "Y." (a fragment the recogniser made of a mumble) was answered with a question; two
// minutes later, after the agent's goodbye, another mumble came out as "Y." again.
//
// In production the second "Y." was never put to the model. The response cache had keyed the first
// reply by the caller's words alone, so it handed the two-minute-old question back as this turn's
// reply ("Response cache hit", 10:56:19): the transcript ended with the earlier exchange copied after
// the goodbye, nothing was spoken (the cache never held audio), and the reply was not added to the
// conversation. Any caller who said the same words twice within five minutes — "Sí.", "No.", "Vale."
// — got that: a silent turn, and a line in the transcript the agent never said.

type textRecordingTTS struct {
	MockTTSProvider
	mu    sync.Mutex
	texts []string
}

func (r *textRecordingTTS) record(text string) {
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
}

func (r *textRecordingTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	r.record(text)
	return r.MockTTSProvider.Synthesize(ctx, text, voice, lang)
}

func (r *textRecordingTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	r.record(text)
	return r.MockTTSProvider.StreamSynthesize(ctx, text, voice, lang, onChunk)
}

func (r *textRecordingTTS) spoke(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.texts {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

func TestReplay20261111_RepeatedWordsAreAnsweredNotReplayed(t *testing.T) {
	const (
		unsure   = "Eh, no lo sé."
		mumble   = "Y."
		leaving  = "No, no, no me tengo que me tengo que ir."
		options  = "¿Para atender llamadas entrantes, hacer llamadas a clientes o gestionar citas?"
		volume   = "¿Cuántas llamadas al día estimas que necesitarías manejar?"
		goodbye  = "Entiendo, disculpa la molestia. Hasta luego."
		newReply = "Hasta luego, que vaya bien."
	)
	tts := &textRecordingTTS{MockTTSProvider: MockTTSProvider{synthesizeResult: make([]byte, 4410)}}
	c := &replayCall{llm: &replayLLM{replies: []string{options, volume, goodbye, newReply}}, log: &replayLog{}}
	cfg := DefaultConfig() // as served: the defaults lokutor_tts runs with
	cfg.SilenceTimeout = time.Hour
	cfg.FirstSpeaker = FirstSpeakerUser
	orch := NewWithLogger(&replaySTT{texts: []string{unsure, mumble, leaving, mumble}}, c.llm, tts,
		NewRMSVAD(0.1, 100*time.Millisecond), cfg, c.log)
	c.stream = orch.NewManagedStream(context.Background(), NewConversationSession("replay-2026-11-11"))
	t.Cleanup(c.stream.Close)
	go func() {
		for ev := range c.stream.Events() {
			if ev.Type == BotResponse {
				if s, ok := ev.Data.(string); ok {
					c.mu.Lock()
					c.botResponses = append(c.botResponses, s)
					c.mu.Unlock()
				}
			}
		}
	}()

	for seq := 1; seq <= 4; seq++ {
		c.utter(seq)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && len(c.sent()) < seq {
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond)

	reqs := c.llm.calls()
	if len(reqs) != 4 {
		t.Fatalf("model asked %d times for 4 caller turns: the second %q was answered without it (sent %q)",
			len(reqs), mumble, c.sent())
	}
	if last, _ := lastSpoken(reqs[3]); last.Role != "user" || last.Content != mumble {
		t.Fatalf("the fourth request must end with the caller's %q, got%s", mumble, dumpMsgs(reqs[3]))
	}
	want := []string{options, volume, goodbye, newReply}
	got := c.sent()
	if len(got) != len(want) {
		t.Fatalf("agent lines sent %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent line %d is %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
	if !tts.spoke(newReply) {
		t.Fatalf("the reply to the second %q was never synthesised: the caller heard nothing", mumble)
	}
	if m, _ := lastSpoken(c.stream.session.GetContextCopy()); m.Role != "assistant" || m.Content != newReply {
		t.Fatalf("the conversation must end with the reply the caller heard, got %+v", m)
	}
	if hits := c.log.find("Response cache hit"); len(hits) != 0 {
		t.Fatalf("a caller turn was answered from the response cache: %+v", hits)
	}
}
