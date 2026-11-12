package orchestrator

import (
	"bytes"
	"context"
	"strings"
	"sync"
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
		"Perfecto.":           "Perfecto…",
		"Hola, soy":           "Hola, soy…",
		"   ":                 "",
		"¡":                   "",
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

// The hangover seed asks the turn processUtterance will build: a continuation's guess is for the joined
// words, against the context the joined turn is answered in, so Await can match it.
func TestSpeculationSeedIsTheJoinedTurn(t *testing.T) {
	const fragment = "¿Qué hora abre la oficina mañana?"
	now := time.Now()

	// No continuation: the fragment, the context as it is.
	ms := newTruthStream(t)
	ms.session.AddMessage("user", "Hola.")
	ms.session.AddMessage("assistant", "¡Hola! ¿En qué te ayudo?")
	if seed, history, cont := ms.speculationSeed(fragment, 2, nil); seed != fragment || cont != "" || len(history) != len(ms.session.GetContextCopy()) {
		t.Fatalf("plain turn: %q %q %d", seed, cont, len(history))
	}

	// A carried fragment: the text join, which is what mergeCarry makes of a settled fragment. The
	// fragment never reached the context, and the carry is left for processUtterance to take.
	ms = newTruthStream(t)
	before := len(ms.session.GetContextCopy())
	ms.mu.Lock()
	ms.carry = &committedUtterance{transcript: "Hola.", endedAt: now.Add(-time.Second)}
	ms.mu.Unlock()
	seed, history, cont := ms.speculationSeed(fragment, 2, nil)
	if seed != "Hola. "+fragment || cont != "Hola." || len(history) != before {
		t.Fatalf("carry: %q %q %d", seed, cont, len(history))
	}
	if ms.carry == nil {
		t.Fatalf("the seed must not consume the carry")
	}

	// Spoken over the reply to a settled utterance: joined, and asked without that utterance or its reply.
	overReply := func(previous string) *ManagedStream {
		ms := newTruthStream(t)
		ms.session.AddMessage("user", "Buenas.")
		ms.session.AddMessage("assistant", "Buenas, dime.")
		ms.session.AddMessage("user", previous)
		ms.session.AddMessage("assistant", "¡Hola! ¿En qué te ayudo?")
		ms.mu.Lock()
		ms.lastUtt = &committedUtterance{transcript: previous, endedAt: now.Add(-2 * time.Second), gen: 5}
		ms.payloadGen = 6
		ms.onset = heardSnapshot{gen: 6, seq: 2, at: now.Add(-time.Second), dur: 400 * time.Millisecond}
		ms.mu.Unlock()
		return ms
	}
	ms = overReply("Hola.")
	seed, history, cont = ms.speculationSeed(fragment, 2, nil)
	if seed != "Hola. "+fragment || cont != "Hola." {
		t.Fatalf("over a reply: %q %q", seed, cont)
	}
	if last := history[len(history)-1]; last.Role != "assistant" || last.Content != "Buenas, dime." {
		t.Fatalf("the merged utterance and its reply must leave the context: %+v", history)
	}
	// The turn processUtterance builds is that context plus the joined words.
	want, _ := replaceLastUserTurn(ms.session.GetContextCopy(), seed)
	if got := append(history, Message{Role: "user", Content: seed}); len(got) != len(want) {
		t.Fatalf("seeded context %d messages, the turn has %d", len(got), len(want))
	}

	// An unsettled utterance joins only through a joint transcription: the fragment, as before.
	ms = overReply("Quiero reservar para")
	if seed, _, cont := ms.speculationSeed(fragment, 2, nil); seed != fragment || cont != "" {
		t.Fatalf("unsettled: %q %q", seed, cont)
	}

	// A note after the utterance: the joined turn would keep it after the words, so the fragment.
	ms = overReply("Hola.")
	ms.session.AddMessage("system", "Retrieved: horario 9-18.")
	if seed, _, cont := ms.speculationSeed(fragment, 2, nil); seed != fragment || cont != "" {
		t.Fatalf("note in the tail: %q %q", seed, cont)
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

func TestPlayoutTimeline_ShiftFromIsAPause(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var p playoutTimeline
	p.schedule(1, 1, "uno dos tres cuatro", t0, time.Second)
	// Paused at 0.5 s for 2 s: the second half plays from 2.5 s to 3 s.
	p.shiftFrom(t0.Add(500*time.Millisecond), 2*time.Second)
	if text, dur, _ := p.heard(t0.Add(2 * time.Second)); text != "uno dos" || dur != 500*time.Millisecond {
		t.Fatalf("during the pause: %q %v", text, dur)
	}
	if text, _, complete := p.heard(t0.Add(3 * time.Second)); !complete || text != "uno dos tres cuatro" {
		t.Fatalf("after the pause: %q complete=%v", text, complete)
	}
	if !p.end.Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("end not shifted: %v", p.end.Sub(t0))
	}
}

func TestAcceptMerged(t *testing.T) {
	if acceptMerged("Oh", "Gracias.") {
		t.Fatalf("a merge shorter than the continuation alone is worse, not better")
	}
	if !acceptMerged("Bueno, esto no ha parado, espérate. Hola, sí.", "I see.") {
		t.Fatalf("a longer joint transcription is the merge")
	}
	if acceptMerged("", "x") {
		t.Fatalf("empty")
	}
}

// On a transport that pauses its queue, speaking over a reply that has finished synthesizing but
// is still playing is a tentative barge-in, and a false alarm resumes it (BotResumed "speaking")
// rather than leaving the rest of the reply silent.
func TestManagedStream_SpeechOverPlayoutIsTentativeOnPausingTransport(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{responses: []string{"Uno dos tres cuatro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("pause"))
	defer stream.Close()
	stream.SetPlaybackRate(16000)
	stream.SetTransportPausesOnBargeIn(true)

	stream.runLLMAndTTS(context.Background(), "cuenta") // synthesis done; one second still to play
	time.Sleep(300 * time.Millisecond)
	stream.mu.Lock()
	idle := stream.state == StateIdle
	stream.mu.Unlock()
	if !idle {
		t.Fatalf("expected Idle after synthesis")
	}
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	pending := stream.pendingBargeIn && stream.pendingBargeGen == stream.payloadGen
	heard := stream.onset.dur
	stream.vadSpeaking = false
	stream.mu.Unlock()
	if !pending {
		t.Fatalf("speech over playout must open a tentative barge-in on a pausing transport")
	}
	if heard < 200*time.Millisecond || heard > 500*time.Millisecond {
		t.Fatalf("heard at onset = %v, want ~300ms", heard)
	}
	stream.resolvePendingBargeIn()
	ev := waitForEventType(t, stream, BotResumed, 2*time.Second)
	if s, _ := ev.Data.(string); s != "speaking" {
		t.Fatalf("BotResumed data = %v", ev.Data)
	}
}

// A rejected noise utterance must not leave the stream in the StateProcessing its own onVADEnd set:
// the next utterance would be taken for a barge-in on a reply that does not exist, and a one-word
// answer dropped under the barge-in word count.
func TestRejectedNoiseRestoresIdleOnlyWhenItOwnsProcessing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inflight int
		want     StreamState
	}{
		{"alone", 1, StateIdle},
		{"another utterance in flight", 2, StateProcessing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.SilenceTimeout = 0
			// A hesitation: discarded in any language, with or without the floor. ("Thank you." on a
			// Spanish call used to be the example, but with the floor it now asks the caller to repeat.)
			orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "Mm."}, &sequencedLLM{}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
			session := NewConversationSession("noise-state")
			session.CurrentLanguage = LanguageEs
			stream := orch.NewManagedStream(context.Background(), session)
			defer stream.Close()
			stream.mu.Lock()
			stream.state = StateProcessing
			stream.utteranceSeq = 1
			stream.inflightUtterances = tc.inflight
			stream.mu.Unlock()
			stream.processUtterance(make([]byte, 16000), 400*time.Millisecond, 1)
			stream.mu.Lock()
			got := stream.state
			stream.mu.Unlock()
			if got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// drainUntil collects events for d and reports which types arrived.
func drainUntil(stream *ManagedStream, d time.Duration) map[EventType]OrchestratorEvent {
	seen := map[EventType]OrchestratorEvent{}
	deadline := time.After(d)
	for {
		select {
		case ev := <-stream.Events():
			seen[ev.Type] = ev
		case <-deadline:
			return seen
		}
	}
}

func playThroughStream(t *testing.T) *ManagedStream {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{responses: []string{"Uno dos tres cuatro."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("playthrough"))
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	stream.SetTransportPlaysThroughBargeIn(true)
	stream.runLLMAndTTS(context.Background(), "cuenta") // synthesis done; one second still to play
	time.Sleep(300 * time.Millisecond)
	drainUntil(stream, 50*time.Millisecond)
	return stream
}

// 2026-09-23, web: a 217 ms blip 1 s into the greeting, transcribed as nothing, cut the greeting for
// good because the browser was told to stop at the onset. On a play-through transport the blip is a
// tentative barge-in: nothing is stopped, the reply is not cut, and the false alarm is only a status.
func TestManagedStream_PlayThrough_BlipNeverCutsTheReply(t *testing.T) {
	stream := playThroughStream(t)
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	pending := stream.pendingBargeIn && stream.pendingBargeGen == stream.payloadGen
	stillPlaying := stream.playout.end.After(time.Now())
	stream.vadSpeaking = false
	stream.mu.Unlock()
	if !pending {
		t.Fatalf("speech over playout must be a tentative barge-in on a play-through transport")
	}
	if !stillPlaying {
		t.Fatalf("the onset cut the playout timeline; the transport is still playing")
	}
	stream.stopPlayThroughIfDue(time.Now()) // 0 ms of speech: not due
	stream.resolvePendingBargeIn()
	seen := drainUntil(stream, 300*time.Millisecond)
	for _, bad := range []EventType{StopPlayback, Interrupted, BotResponseTruncated} {
		if _, ok := seen[bad]; ok {
			t.Fatalf("a false alarm emitted %v", bad)
		}
	}
	if ev, ok := seen[BotResumed]; !ok || ev.Data != "speaking" {
		t.Fatalf("BotResumed = %+v (seen %v)", ev, ok)
	}
}

// A caller who keeps talking over the reply stops it once they pass the window, and what they heard
// is settled at that moment — not at the onset, since the transport kept playing until told.
func TestManagedStream_PlayThrough_SustainedSpeechStopsPlayback(t *testing.T) {
	t.Setenv("BARGE_IN_PLAYTHROUGH_MS", "150")
	stream := playThroughStream(t)
	stream.onVADStart(StateIdle)
	stream.mu.Lock()
	onsetHeard := stream.onset.dur
	stream.mu.Unlock()

	time.Sleep(200 * time.Millisecond)
	stream.stopPlayThroughIfDue(time.Now())
	seen := drainUntil(stream, 300*time.Millisecond)
	if _, ok := seen[StopPlayback]; !ok {
		t.Fatalf("sustained speech over the reply did not stop playback (seen %v)", seen)
	}
	stream.mu.Lock()
	stopped, heard := stream.onset.stopped, stream.onset.dur
	cut := !stream.playout.end.After(time.Now())
	stream.mu.Unlock()
	if !stopped || !cut {
		t.Fatalf("stopped=%v cut=%v", stopped, cut)
	}
	if heard < onsetHeard+150*time.Millisecond {
		t.Fatalf("heard %v at the stop, %v at the onset: the reply played on in between", heard, onsetHeard)
	}
	if _, ok := seen[BotResponseTruncated]; !ok {
		t.Fatalf("the cut reply was not corrected to what was heard")
	}
	// Once stopped it stays stopped: a second check emits nothing.
	stream.stopPlayThroughIfDue(time.Now())
	if _, ok := drainUntil(stream, 100*time.Millisecond)[StopPlayback]; ok {
		t.Fatalf("StopPlayback emitted twice")
	}
}

// 2026-09-23, English web session: "Okay." said with the floor was discarded as noise, and so was
// every repeat. With nothing to resume it is the caller's turn.
func TestProcessUtterance_EnglishShortReplyWithTheFloorIsATurn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "Okay."}, &sequencedLLM{responses: []string{"Great."}}, fixedAudioTTS{}, nil, cfg, &NoOpLogger{})
	session := NewConversationSession("okay")
	session.CurrentLanguage = LanguageEn
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()
	stream.mu.Lock()
	stream.state = StateProcessing
	stream.utteranceSeq = 1
	stream.inflightUtterances = 1
	stream.mu.Unlock()
	stream.processUtterance(make([]byte, 16000), 413*time.Millisecond, 1)
	ev := waitForEventType(t, stream, TranscriptFinal, 3*time.Second)
	if ev.Data != "Okay." {
		t.Fatalf("TranscriptFinal = %v", ev.Data)
	}
}

// The recogniser writes a joint transcription's sentences with no space between them; that is still
// more words than the continuation alone.
func TestAcceptMergedSentencesRunTogether(t *testing.T) {
	if !acceptMerged("Hola.¿Qué hora abre la oficina mañana?", "¿Qué hora abre la oficina mañana?") {
		t.Fatal(`"Hola.¿Qué hora…" joins both halves and must be accepted`)
	}
	if got := spaceSentences("Hola.¿Qué hora? Sí.Vale, 3.5 euros"); got != "Hola. ¿Qué hora? Sí. Vale, 3.5 euros" {
		t.Fatalf("spaceSentences: %q", got)
	}
}

func TestSettledOnItsOwn(t *testing.T) {
	for frag, want := range map[string]bool{
		"Hola.": true, "Um.": true, "Vale, sí.": true, "¿Sí?": true, "": true,
		"Hola": false, "I'd like to book a flight to": false, "Mi nombre es Pito,": false,
	} {
		if got := settledOnItsOwn(frag); got != want {
			t.Errorf("settledOnItsOwn(%q) = %v, want %v", frag, got, want)
		}
	}
}

// The two halves are joined with nothing between them: each already carries the caller's real pause, and a
// block of digital zeros made the recogniser lose the second half (2026-11-12, telnyx: the joint pass came
// back as the first half's words).
func TestJoinUtteranceAudioAddsNoSilence(t *testing.T) {
	prev := []byte{1, 2, 3, 4}
	next := []byte{5, 6}
	got := joinUtteranceAudio(prev, next)
	if string(got) != string([]byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("joined = %v", got)
	}
}

// slowSTT answers after delay, or when its context is cancelled.
type slowSTT struct {
	delay time.Duration
	text  string
}

func (s *slowSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	select {
	case <-time.After(s.delay):
		return TranscriptionResult{Text: s.text}, nil
	case <-ctx.Done():
		return TranscriptionResult{}, ctx.Err()
	}
}
func (s *slowSTT) Name() string { return "slowSTT" }

func TestMergeCarry(t *testing.T) {
	base := &committedUtterance{audio: make([]byte, 3200), transcript: "Quería reservar un vuelo a", endedAt: time.Now()}
	own := "Madrid el viernes."
	newStream := func(stt STTProvider) *ManagedStream {
		orch := New(stt, &MockLLMProvider{completeResult: "ok"}, &MockTTSProvider{}, DefaultConfig())
		return orch.NewManagedStream(context.Background(), NewConversationSession("merge-carry"))
	}

	// No joint pass for this fragment: joined as text, at once.
	ms := newStream(&slowSTT{delay: time.Hour})
	defer ms.Close()
	short := &committedUtterance{audio: make([]byte, 3200), transcript: "Hola.", endedAt: time.Now()}
	if got, _ := ms.mergeCarry(context.Background(), short, make([]byte, 3200), "¿Qué hora abre?", nil); got != "Hola. ¿Qué hora abre?" {
		t.Fatalf("no joint pass: %q", got)
	}

	// A fragment the recogniser closed with a full stop is transcribed together with its continuation too:
	// "Manuel de." + "Fisioterapia es Manolo." was joined as text because of that full stop, where the two
	// halves transcribed together read "Manolo de Fisioterapia es Manolo." (2026-11-11, telnyx).
	manolo := newStream(&slowSTT{delay: 10 * time.Millisecond, text: "Manolo de Fisioterapia es Manolo."})
	defer manolo.Close()
	frag := &committedUtterance{audio: make([]byte, 3200), transcript: "Manuel de.", endedAt: time.Now()}
	j := manolo.startJointTranscription(context.Background(), frag, make([]byte, 3200))
	if got, _ := manolo.mergeCarry(context.Background(), frag, make([]byte, 3200), "Fisioterapia es Manolo.", j); got != "Manolo de Fisioterapia es Manolo." {
		t.Fatalf("closed fragment: %q", got)
	}

	// A joint pass slower than the budget is not waited for: the two halves are joined as text, and the
	// first half is kept.
	slow := newStream(&slowSTT{delay: time.Second, text: "Quería reservar un vuelo a Madrid el viernes."})
	defer slow.Close()
	j = slow.startJointTranscription(context.Background(), base, make([]byte, 3200))
	start := time.Now()
	got, _ := slow.mergeCarry(context.Background(), base, make([]byte, 3200), own, j)
	if got != "Quería reservar un vuelo a Madrid el viernes." {
		t.Fatalf("slow joint pass: %q", got)
	}
	if waited := time.Since(start); waited > jointWaitBudget+100*time.Millisecond {
		t.Fatalf("waited %v for a joint pass past its budget of %v", waited, jointWaitBudget)
	}

	// A joint pass ready in time is used.
	fast := newStream(&slowSTT{delay: 10 * time.Millisecond, text: "Quería reservar un vuelo a Madrid, el viernes."})
	defer fast.Close()
	j = fast.startJointTranscription(context.Background(), base, make([]byte, 3200))
	if got, _ := fast.mergeCarry(context.Background(), base, make([]byte, 3200), own, j); got != "Quería reservar un vuelo a Madrid, el viernes." {
		t.Fatalf("fast joint pass: %q", got)
	}
}

func TestMergeBarge(t *testing.T) {
	newStream := func(stt STTProvider) *ManagedStream {
		orch := New(stt, &MockLLMProvider{completeResult: "ok"}, &MockTTSProvider{}, DefaultConfig())
		return orch.NewManagedStream(context.Background(), NewConversationSession("merge-barge"))
	}
	hola := &committedUtterance{audio: make([]byte, 3200), transcript: "Hola.", endedAt: time.Now()}

	// A previous utterance that settled on its own, with no joint pass: joined as text, no wait.
	ms := newStream(&slowSTT{delay: time.Hour})
	defer ms.Close()
	start := time.Now()
	got, joined, ok := ms.mergeBarge(context.Background(), hola, make([]byte, 3200), "¿Qué hora abre la oficina mañana?", nil)
	if !ok || got != "Hola. ¿Qué hora abre la oficina mañana?" || len(joined) != 6400 {
		t.Fatalf("settled fragment: %q ok=%v len=%d", got, ok, len(joined))
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		t.Fatalf("settled fragment waited %v", waited)
	}

	// The same with a joint pass ready in time: its words are used ("Manuel de." over a reply is no more
	// settled than the same fragment held by the end-of-turn wait).
	manolo := newStream(&slowSTT{delay: 10 * time.Millisecond, text: "Sí. Manolito Gafotas Gmail. ¿Com?"})
	defer manolo.Close()
	si := &committedUtterance{audio: make([]byte, 3200), transcript: "Sí. Manolitografotas.", endedAt: time.Now()}
	j := manolo.startJointTranscription(context.Background(), si, make([]byte, 3200))
	if got, _, ok := manolo.mergeBarge(context.Background(), si, make([]byte, 3200), "¿Gmail, com?", j); !ok || got != "Sí. Manolito Gafotas Gmail. ¿Com?" {
		t.Fatalf("settled fragment with a joint pass: %q ok=%v", got, ok)
	}
	// ... and when the joint pass is not used, joined as text as before.
	rejected := newStream(&slowSTT{delay: 10 * time.Millisecond, text: "Sí."})
	defer rejected.Close()
	j = rejected.startJointTranscription(context.Background(), hola, make([]byte, 3200))
	if got, _, ok := rejected.mergeBarge(context.Background(), hola, make([]byte, 3200), "¿Qué hora abre?", j); !ok || got != "Hola. ¿Qué hora abre?" {
		t.Fatalf("settled fragment, joint pass not used: %q ok=%v", got, ok)
	}

	// An open fragment with a joint pass slower than the budget keeps the turns separate, within budget.
	base := &committedUtterance{audio: make([]byte, 3200), transcript: "Quería reservar un vuelo a", endedAt: time.Now()}
	own := "Madrid el viernes."
	slow := newStream(&slowSTT{delay: time.Second, text: "Quería reservar un vuelo a Madrid el viernes."})
	defer slow.Close()
	j = slow.startJointTranscription(context.Background(), base, make([]byte, 3200))
	start = time.Now()
	if _, _, ok := slow.mergeBarge(context.Background(), base, make([]byte, 3200), own, j); ok {
		t.Fatal("a joint pass past its budget must not be used")
	}
	if waited := time.Since(start); waited > jointWaitBudget+100*time.Millisecond {
		t.Fatalf("waited %v for a joint pass past its budget of %v", waited, jointWaitBudget)
	}

	// No joint pass started for this base: separate, immediately.
	if _, _, ok := slow.mergeBarge(context.Background(), base, make([]byte, 3200), own, nil); ok {
		t.Fatal("no joint pass means separate turns")
	}

	// A joint pass ready in time is used.
	fast := newStream(&slowSTT{delay: 10 * time.Millisecond, text: "Quería reservar un vuelo a Madrid, el viernes."})
	defer fast.Close()
	j = fast.startJointTranscription(context.Background(), base, make([]byte, 3200))
	if got, _, ok := fast.mergeBarge(context.Background(), base, make([]byte, 3200), own, j); !ok || got != "Quería reservar un vuelo a Madrid, el viernes." {
		t.Fatalf("fast joint pass: %q ok=%v", got, ok)
	}
}

// finishedJoint is a joint pass over base that has already returned text.
func finishedJoint(base *committedUtterance, text string) *jointTranscription {
	j := &jointTranscription{base: base, cancel: func() {}, done: make(chan struct{}), text: text, speculative: true}
	close(j.done)
	return j
}

// The hangover seed asks the words the turn will carry when its joint pass has finished and is accepted: seeded
// with the halves joined as text while the turn used the joint pass's words, a guess over a reply was used on 4%
// of such turns (14 days to 2026-11-12) against 54% of those joined as text.
func TestSpeculationSeedUsesTheJointPass(t *testing.T) {
	now := time.Now()

	// A carried fragment closed by the recogniser ("Manuel de."): the joint pass's words.
	ms := newTruthStream(t)
	carry := &committedUtterance{transcript: "Manuel de.", endedAt: now.Add(-time.Second)}
	ms.mu.Lock()
	ms.carry = carry
	ms.mu.Unlock()
	if seed, _, cont := ms.speculationSeed("Fisioterapia es Manolo.", 2, finishedJoint(carry, "Manolo de Fisioterapia es Manolo.")); seed != "Manolo de Fisioterapia es Manolo." || cont != "Manuel de." {
		t.Fatalf("carry: %q %q", seed, cont)
	}
	// A joint pass that lost the continuation is not the turn's words: the text join, as the turn will do.
	if seed, _, _ := ms.speculationSeed("Fisioterapia es Manolo.", 2, finishedJoint(carry, "Manuel de.")); seed != "Manuel de. Fisioterapia es Manolo." {
		t.Fatalf("carry, joint not used: %q", seed)
	}
	// One still running is not waited for here.
	running := &jointTranscription{base: carry, cancel: func() {}, done: make(chan struct{})}
	if seed, _, _ := ms.speculationSeed("Fisioterapia es Manolo.", 2, running); seed != "Manuel de. Fisioterapia es Manolo." {
		t.Fatalf("carry, joint running: %q", seed)
	}

	// Over the reply to an open utterance: joined through the joint pass, asked without that utterance and its reply.
	ms = newTruthStream(t)
	ms.session.AddMessage("user", "Quiero reservar para")
	ms.session.AddMessage("assistant", "¿Para cuándo?")
	prev := &committedUtterance{transcript: "Quiero reservar para", endedAt: now.Add(-2 * time.Second), gen: 5}
	ms.mu.Lock()
	ms.lastUtt = prev
	ms.payloadGen = 6
	ms.onset = heardSnapshot{gen: 6, seq: 2, at: now.Add(-time.Second), dur: 400 * time.Millisecond}
	ms.mu.Unlock()
	seed, history, cont := ms.speculationSeed("el viernes.", 2, finishedJoint(prev, "Quiero reservar para el viernes."))
	if seed != "Quiero reservar para el viernes." || cont != "Quiero reservar para" || len(history) != 0 {
		t.Fatalf("over a reply: %q %q %+v", seed, cont, history)
	}
	// Its joint pass not used: a turn of its own, the fragment is the guess.
	if seed, _, cont := ms.speculationSeed("el viernes.", 2, finishedJoint(prev, "Quiero")); seed != "el viernes." || cont != "" {
		t.Fatalf("over a reply, joint not used: %q %q", seed, cont)
	}
}

// The joint pass the hangover started is the turn's when the turn uses that pass's transcript, and stopped when
// it does not.
func TestSpeculativeJointIsHandedToTheTurn(t *testing.T) {
	base := &committedUtterance{transcript: "Manuel de."}
	other := &committedUtterance{transcript: "Hola."}
	cancelled := 0
	newJoint := func(b *committedUtterance) *jointTranscription {
		return &jointTranscription{base: b, cancel: func() { cancelled++ }, done: make(chan struct{})}
	}

	var s specSTT
	attempt, _ := s.start(100, 2)
	j := newJoint(base)
	s.setJoint(attempt, j)
	if !s.hasJointFor(2, base) || s.hasJointFor(3, base) || s.hasJointFor(2, other) {
		t.Fatal("hasJointFor must match the utterance and its earlier half")
	}
	if s.takeJoint(2, other) != nil || s.takeJoint(2, base) != j || s.takeJoint(2, base) != nil {
		t.Fatal("takeJoint hands the pass over once, for its own base")
	}
	s.invalidate()
	if cancelled != 0 {
		t.Fatal("a joint pass the turn took must keep running after the attempt is invalidated")
	}

	// Not taken: stopped with the attempt.
	attempt, _ = s.start(100, 3)
	s.setJoint(attempt, newJoint(base))
	s.invalidate()
	if cancelled != 1 || s.hasJointFor(3, base) {
		t.Fatalf("an untaken joint pass must stop with its attempt (cancelled %d)", cancelled)
	}

	// For an attempt no longer in flight: stopped at once.
	s.setJoint(attempt, newJoint(base))
	if cancelled != 2 {
		t.Fatal("a joint pass for a stale attempt must be stopped")
	}
}

// countingLengthSTT answers by audio length and counts the calls for each length.
type countingLengthSTT struct {
	mu    sync.Mutex
	byLen map[int]string
	calls map[int]int
}

func (s *countingLengthSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[len(audio)]++
	return TranscriptionResult{Text: s.byLen[len(audio)]}, nil
}
func (s *countingLengthSTT) Name() string { return "countingLengthSTT" }

// lastUserLLM records the last user message of every request.
type lastUserLLM struct {
	mu    sync.Mutex
	users []string
	reply string
}

func (l *lastUserLLM) Complete(ctx context.Context, messages []Message, tools []Tool) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			l.users = append(l.users, messages[i].Content)
			break
		}
	}
	return l.reply, nil
}
func (l *lastUserLLM) Name() string { return "lastUserLLM" }

// A held fragment and its continuation, end to end through the hangover: the joint pass starts with the
// speculative transcription, the seed asks the model the joint pass's words, and the turn takes that pass
// (no second joint request, no wait) and answers with the speculative reply.
func TestHeldFragmentIsTranscribedTogetherInTheHangover(t *testing.T) {
	const ms16 = 32 // bytes per ms at 16 kHz PCM16
	carryAudio := bytes.Repeat([]byte{1}, 1200*ms16)
	leadIn := bytes.Repeat([]byte{2}, 300*ms16)
	speech := bytes.Repeat([]byte{3}, 900*ms16)
	snapshot := append(append([]byte(nil), leadIn...), speech...)
	final := append(append([]byte(nil), snapshot...), make([]byte, 192*ms16)...) // + the hangover's silence
	const merged = "Manolo de Fisioterapia es Manolo."
	stt := &countingLengthSTT{calls: map[int]int{}, byLen: map[int]string{
		len(snapshot):                   "Fisioterapia es Manolo.",
		len(carryAudio) + len(snapshot): merged,
		len(final):                      "Fisioterapia es Manolo.",
		len(carryAudio) + len(final):    merged,
	}}
	llm := &lastUserLLM{reply: "Encantada, Manolo."}
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 16000
	orch := NewWithAllLayers(stt, llm, fixedAudioTTS{}, &silentVAD{}, cfg, &NoOpLogger{})
	session := NewConversationSession("manolo")
	session.CurrentLanguage = LanguageEs
	stream := orch.NewManagedStream(context.Background(), session)
	t.Cleanup(stream.Close)
	stream.vad = &silentVAD{} // NewManagedStream clones the VAD, and the clone is a plain RMSVAD
	if !stream.hangoverSeedActive() {
		t.Fatal("the hangover seed must be active: speculative STT chained into the model")
	}

	stream.mu.Lock()
	stream.carry = &committedUtterance{audio: carryAudio, transcript: "Manuel de.", endedAt: time.Now(), seq: 1}
	stream.utteranceSeq = 1
	stream.speechLeadIn = leadIn
	stream.speechAudioBuf = append([]byte(nil), speech...)
	stream.mu.Unlock()

	stream.maybeSpeculateSTT() // the hangover's first silent frame
	waitUntil(t, "the hangover seed", 2*time.Second, func() bool {
		llm.mu.Lock()
		defer llm.mu.Unlock()
		return len(llm.users) > 0
	})
	llm.mu.Lock()
	seed := llm.users[0]
	llm.mu.Unlock()
	if seed != merged {
		t.Fatalf("the seed asked %q, want the joint pass's words %q", seed, merged)
	}

	// End of turn.
	stream.mu.Lock()
	stream.state, stream.utteranceSeq, stream.inflightUtterances = StateProcessing, 2, 1
	stream.mu.Unlock()
	stream.processUtterance(final, 1092*time.Millisecond, 2)
	if ev := waitForEventType(t, stream, TranscriptFinal, 3*time.Second); ev.Data != merged {
		t.Fatalf("TranscriptFinal = %v, want %q", ev.Data, merged)
	}

	stt.mu.Lock()
	defer stt.mu.Unlock()
	if stt.calls[len(carryAudio)+len(snapshot)] != 1 || stt.calls[len(carryAudio)+len(final)] != 0 || stt.calls[len(final)] != 0 {
		t.Fatalf("recognition calls by length: %v (want the snapshot and its joint pass once each, nothing else)", stt.calls)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.users) != 1 {
		t.Fatalf("model asked %d times (%q): the turn must use the speculative reply", len(llm.users), llm.users)
	}
}
