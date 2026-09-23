package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPlayoutTimeline_HeardIsWhatHadPlayed(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var p playoutTimeline
	// Two sentences of one second each, handed to the transport at once: they queue back to back.
	p.schedule(7, 1, "Hola, soy su asistente.", t0, time.Second)
	p.schedule(7, 2, "¿Cuándo le gustaría llegar y cuántas noches?", t0, time.Second)

	text, dur, complete := p.heard(t0.Add(500 * time.Millisecond))
	if text != "Hola, soy" || complete || dur != 500*time.Millisecond {
		t.Fatalf("half of the first sentence: got %q dur=%v complete=%v", text, dur, complete)
	}
	text, _, complete = p.heard(t0.Add(1500 * time.Millisecond))
	if text != "Hola, soy su asistente. ¿Cuándo le gustaría" || complete {
		t.Fatalf("first sentence and half the second: got %q complete=%v", text, complete)
	}
	text, _, complete = p.heard(t0.Add(3 * time.Second))
	if !complete || !strings.HasSuffix(text, "noches?") {
		t.Fatalf("everything: got %q complete=%v", text, complete)
	}
	if text, _, _ := p.heard(t0); text != "" {
		t.Fatalf("nothing played yet: got %q", text)
	}
}

func TestPlayoutTimeline_LateChunkLeavesAGapNotHeardAudio(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var p playoutTimeline
	p.schedule(1, 1, "uno dos tres cuatro", t0, 500*time.Millisecond)
	// Synthesis fell behind: the second half arrives a second after the first finished playing.
	p.schedule(1, 1, "uno dos tres cuatro", t0.Add(1500*time.Millisecond), 500*time.Millisecond)
	// At 1.2 s only the first half has played (the gap is silence, not speech).
	text, dur, _ := p.heard(t0.Add(1200 * time.Millisecond))
	if dur != 500*time.Millisecond || text != "uno dos" {
		t.Fatalf("got %q dur=%v", text, dur)
	}
}

func TestPlayoutTimeline_CutAtDropsFlushedAudio(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var p playoutTimeline
	p.schedule(1, 1, "a b c d", t0, time.Second)
	p.cutAt(t0.Add(250 * time.Millisecond))
	// Resumed after a false alarm: the rest is scheduled from the resume point.
	p.schedule(1, 2, "e f", t0.Add(2*time.Second), 500*time.Millisecond)
	text, dur, complete := p.heard(t0.Add(5 * time.Second))
	if !complete || dur != 750*time.Millisecond || text != "a b c d e f" {
		// The cut segment counts as fully played: all of its remaining audio was flushed.
		t.Fatalf("got %q dur=%v complete=%v", text, dur, complete)
	}
	if p.gen != 1 {
		t.Fatalf("gen changed")
	}
	p.schedule(2, 9, "nueva", t0.Add(6*time.Second), time.Second)
	if len(p.segs) != 1 || p.segs[0].text != "nueva" {
		t.Fatalf("a new generation must start a new timeline: %+v", p.segs)
	}
}

func TestWithInterruptionMark(t *testing.T) {
	for in, want := range map[string]string{
		"¿Cuándo le gustaría": "¿Cuándo le gustaría…",
		"Perfecto.":            "Perfecto…",
		"Hola, soy":            "Hola, soy…",
		"   ":                  "",
		"¡":                    "",
	} {
		if got := withInterruptionMark(in); got != want {
			t.Errorf("withInterruptionMark(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReplaceLastUserTurn(t *testing.T) {
	base := []Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "Pito."},
		{Role: "system", Content: "[Knowledge base context]"},
		{Role: "assistant", Content: "¡Encantado, Pito!"},
	}
	out, ok := replaceLastUserTurn(base, "Pito Grillo.")
	if !ok || len(out) != 3 || out[1].Content != "Pito Grillo." || out[2].Role != "system" {
		t.Fatalf("merge must replace the user turn, keep system notes and drop the reply: ok=%v %+v", ok, out)
	}
	if base[1].Content != "Pito." {
		t.Fatalf("input slice mutated")
	}
	withTool := append(append([]Message{}, base[:2]...), Message{Role: "tool", Content: "{}"})
	if _, ok := replaceLastUserTurn(withTool, "x"); ok {
		t.Fatalf("a turn that ran a tool must not be merged")
	}
	toolCall := append(append([]Message{}, base[:2]...), Message{Role: "assistant", ToolCalls: []interface{}{1}})
	if _, ok := replaceLastUserTurn(toolCall, "x"); ok {
		t.Fatalf("a turn whose reply called a tool must not be merged")
	}
	if _, ok := replaceLastUserTurn([]Message{{Role: "system", Content: "p"}}, "x"); ok {
		t.Fatalf("no user turn to merge into")
	}
}

func newTruthStream(t *testing.T) *ManagedStream {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{}, &blockingAfterFirstTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("truth"))
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	return stream
}

func TestSpokenTruth_ContextRewrittenToWhatWasHeard(t *testing.T) {
	ms := newTruthStream(t)
	ms.session.AddMessage("user", "Quiero una reserva.")
	ms.session.AddMessage("assistant", "¿Cuándo le gustaría llegar y cuántas noches desea quedarse?")
	ms.applySpokenTruthToContext(3, TruncatedResponse{Spoken: "¿Cuándo le gustaría…"})
	ctx := ms.session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Role != "assistant" || last.Content != "¿Cuándo le gustaría…" {
		t.Fatalf("reply not truncated: %+v", last)
	}

	// Nothing heard: the reply goes.
	ms.session.AddMessage("user", "Otra cosa.")
	ms.session.AddMessage("assistant", "Claro, dígame.")
	ms.applySpokenTruthToContext(4, TruncatedResponse{Spoken: ""})
	ctx = ms.session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Role != "user" {
		t.Fatalf("unheard reply not removed: %+v", last)
	}
	// ... and never an earlier turn's reply.
	found := false
	for _, m := range ctx {
		if m.Content == "¿Cuándo le gustaría…" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an earlier reply was removed")
	}

	// Cut before the reply committed itself: the heard part is added.
	ms.applySpokenTruthToContext(5, TruncatedResponse{Spoken: "Un momento…"})
	ctx = ms.session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Content != "Un momento…" {
		t.Fatalf("heard part not recorded: %+v", last)
	}
}

func TestCommitStreamedReply_OrderedAgainstInterrupt(t *testing.T) {
	ms := newTruthStream(t)
	ms.session.AddMessage("user", "hola")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// The interruption recorded what was heard first: the cancelled stream adds nothing.
	ms.applySpokenTruthToContext(9, TruncatedResponse{Spoken: "Hola, soy…"})
	ms.mu.Lock()
	ms.playout.schedule(9, 1, "Hola, soy su asistente.", time.Now(), time.Second)
	ms.mu.Unlock()
	ms.commitStreamedReply(cancelled, 9, "Hola, soy su asistente. ¿En qué le ayudo?", "hola")
	n := 0
	for _, m := range ms.session.GetContextCopy() {
		if m.Role == "assistant" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly the heard reply in context, got %d assistant messages", n)
	}

	// Cancelled some other way (no interruption recorded for this generation): what reached the
	// caller is recorded provisionally.
	ms.session.AddMessage("user", "otra")
	ms.mu.Lock()
	ms.playout.schedule(10, 1, "Primera frase.", time.Now(), time.Second)
	ms.mu.Unlock()
	ms.commitStreamedReply(cancelled, 10, "Primera frase. Segunda frase.", "otra")
	ctx := ms.session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Content != "Primera frase." {
		t.Fatalf("provisional reply: %+v", last)
	}
}

func TestSpokenTruthLocked(t *testing.T) {
	ms := newTruthStream(t)
	now := time.Now()
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.payloadGen = 4
	if _, ok := ms.spokenTruthLocked(now); ok {
		t.Fatalf("no reply sent or played: nothing to report")
	}
	ms.lastReply = replyRecord{gen: 4, text: "Uno dos tres cuatro."}
	ms.playout.schedule(4, 1, "Uno dos tres cuatro.", now, time.Second)
	ms.onset = heardSnapshot{gen: 4, at: now.Add(500 * time.Millisecond)}
	ms.onset.text, ms.onset.dur, ms.onset.complete = ms.playout.heard(ms.onset.at)
	tr, ok := ms.spokenTruthLocked(now.Add(2 * time.Second))
	if !ok || tr.Spoken != "Uno dos…" || tr.Full != "Uno dos tres cuatro." || !tr.Emitted || tr.HeardMs != 500 {
		t.Fatalf("heard at the onset, not at confirmation: %+v", tr)
	}
	ms.mergeOnConfirm = true
	if tr, _ := ms.spokenTruthLocked(now); tr.Spoken != "" {
		t.Fatalf("a merged continuation drops the reply: %+v", tr)
	}
	ms.mergeOnConfirm = false
	ms.onset = heardSnapshot{gen: 4, at: now.Add(3 * time.Second)}
	ms.onset.text, ms.onset.dur, ms.onset.complete = ms.playout.heard(ms.onset.at)
	if tr, _ := ms.spokenTruthLocked(now); tr.Spoken != "Uno dos tres cuatro." {
		t.Fatalf("heard in full: no interruption mark: %+v", tr)
	}
}

func TestContinuationBase(t *testing.T) {
	ms := newTruthStream(t)
	now := time.Now()
	ms.mu.Lock()
	defer ms.mu.Unlock()

	// A fragment abandoned by the mid-thought wait is always the base of what follows it.
	ms.carry = &committedUtterance{transcript: "Quiero que,", endedAt: now.Add(-2 * time.Second)}
	if b, barge := ms.continuationBaseLocked(now, 1, false); b == nil || barge || b.transcript != "Quiero que," {
		t.Fatalf("carry: %+v %v", b, barge)
	}
	if ms.carry != nil {
		t.Fatalf("carry must be consumed")
	}
	ms.carry = &committedUtterance{transcript: "old", endedAt: now.Add(-time.Minute)}
	if b, _ := ms.continuationBaseLocked(now, 1, false); b != nil {
		t.Fatalf("a stale fragment is not a continuation base")
	}

	// Speaking over the reply to the previous utterance, having heard almost none of it.
	ms.lastUtt = &committedUtterance{transcript: "Pito.", endedAt: now.Add(-3 * time.Second), gen: 5}
	ms.payloadGen = 6
	ms.onset = heardSnapshot{gen: 6, at: now.Add(-time.Second), dur: 400 * time.Millisecond}
	if b, barge := ms.continuationBaseLocked(now, 2, true); b == nil || !barge {
		t.Fatalf("barely-heard reply: expected a merge")
	}
	if b, _ := ms.continuationBaseLocked(now, 2, false); b != nil {
		t.Fatalf("not speaking over a reply: a normal new turn")
	}
	// Speaking over a finished reply still playing counts the same, via this utterance's onset.
	ms.onset.seq = 2
	if b, barge := ms.continuationBaseLocked(now, 2, false); b == nil || !barge {
		t.Fatalf("speaking over playout: expected a merge")
	}
	if b, _ := ms.continuationBaseLocked(now, 3, false); b != nil {
		t.Fatalf("an onset belonging to another utterance must not count")
	}
	ms.onset.seq = 0
	ms.onset.dur = 3 * time.Second
	if b, _ := ms.continuationBaseLocked(now, 2, true); b != nil {
		t.Fatalf("the caller heard real words: the interruption is a turn of its own")
	}
	ms.onset.dur = 400 * time.Millisecond
	ms.payloadGen = 5
	ms.onset.gen = 5
	if b, _ := ms.continuationBaseLocked(now, 2, true); b != nil {
		t.Fatalf("the reply being cut is not the answer to the previous utterance")
	}
}

// fixedAudioTTS delivers one second of 16 kHz audio per text, in ten chunks, immediately.
type fixedAudioTTS struct{}

func (fixedAudioTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	return make([]byte, 32000), nil
}
func (fixedAudioTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	for i := 0; i < 10; i++ {
		if err := onChunk(make([]byte, 3200)); err != nil {
			return err
		}
	}
	return nil
}
func (fixedAudioTTS) Abort() error { return nil }
func (fixedAudioTTS) Name() string { return "FixedAudioTTS" }

// The whole path on a real stream: a one-second reply is synthesized (and sent as a BotResponse)
// at once, the caller cuts in half a second into playback, and both the event and the model's
// context say they heard about half of it.
func TestManagedStream_InterruptRecordsWhatWasHeard(t *testing.T) {
	reply := "Uno dos tres cuatro cinco seis siete ocho."
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{responses: []string{reply}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	session := NewConversationSession("truth-e2e")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()
	stream.SetPlaybackRate(16000)
	session.AddMessage("user", "cuenta hasta ocho")

	go stream.runLLMAndTTS(context.Background(), "cuenta hasta ocho")
	waitForEventType(t, stream, BotResponse, 2*time.Second)
	time.Sleep(500 * time.Millisecond)
	stream.Interrupt()
	ev := waitForEventType(t, stream, BotResponseTruncated, 2*time.Second)
	tr, ok := ev.Data.(TruncatedResponse)
	if !ok {
		t.Fatalf("event data: %T", ev.Data)
	}
	if tr.Full != reply || !tr.Emitted || !strings.HasPrefix(tr.Spoken, "Uno dos tres") ||
		!strings.HasSuffix(tr.Spoken, interruptionMark) || strings.Contains(tr.Spoken, "ocho") {
		t.Fatalf("expected about half the reply, cut off: %+v", tr)
	}
	if tr.HeardMs < 400 || tr.HeardMs > 700 {
		t.Fatalf("heard_ms = %d, want ~500", tr.HeardMs)
	}
	ctx := session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Role != "assistant" || last.Content != tr.Spoken {
		t.Fatalf("context must hold what was heard, got %+v", last)
	}
}
