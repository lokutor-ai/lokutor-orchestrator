package orchestrator

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// piecesSTT answers each utterance with the next piece, and a joint pass (audio longer than one piece) with every piece
// heard so far, as a recogniser does.
type piecesSTT struct {
	mu     sync.Mutex
	pieces []string
	n      int
	eou    float64
}

func (s *piecesSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := (len(audio) + pieceBytes/2) / pieceBytes; k > 1 {
		return TranscriptionResult{Text: strings.Join(s.pieces[:min(k, len(s.pieces))], " "), EndOfTurn: s.eou, HasEndOfTurn: true}, nil
	}
	if s.n < len(s.pieces) {
		s.n++
	}
	return TranscriptionResult{Text: s.pieces[s.n-1], EndOfTurn: s.eou, HasEndOfTurn: true}, nil
}
func (s *piecesSTT) Name() string { return "pieces" }

const pieceBytes = 16000 // half a second

func listenStream(t *testing.T, stt STTProvider, llm LLMProvider, tts TTSProvider) (*ManagedStream, *Orchestrator) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SpeculativeLLM = false
	cfg.FirstSpeaker = FirstSpeakerUser
	cfg.SampleRate = 16000
	cfg.EndOfTurnThreshold = 0.5
	cfg.EndOfTurnWaitMs = 50
	orch := NewWithAllLayers(stt, llm, tts, nil, cfg, &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("listen"))
	t.Cleanup(ms.Close)
	go func() {
		for range ms.Events() {
		}
	}()
	return ms, orch
}

// piece plays one utterance as onVADEnd does; pause is how long the caller is silent before the next one starts.
func piece(ms *ManagedStream, pause time.Duration) {
	ms.mu.Lock()
	ms.closeGateLocked() // the caller starts speaking again
	ms.mu.Unlock()
	time.Sleep(50 * time.Millisecond) // and goes on until the piece ends
	ms.mu.Lock()
	ms.utteranceSeq++
	seq := ms.utteranceSeq
	ms.state = StateProcessing
	ms.inflightUtterances++
	ms.mu.Unlock()
	go ms.processUtterance(make([]byte, pieceBytes), 500*time.Millisecond, seq)
	time.Sleep(pause)
}

// closeGateLocked is what the caller resuming does to a turn that waits for more.
func (ms *ManagedStream) closeGateLocked() {
	if ms.confirmationGate != nil {
		close(ms.confirmationGate)
		ms.confirmationGate = nil
	}
}

func userTurns(ms *ManagedStream) []string {
	var out []string
	for _, m := range ms.session.GetContextCopy() {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// Pieces that each read as a finished turn (0.95) are one turn while listening, answered once, with nothing said between
// them; the turn is answered soon after its words are the whole of it.
func TestListeningJoinsDictatedPieces(t *testing.T) {
	stt := &piecesSTT{pieces: []string{"librería", "central", "arroba", "a", "gmail punto com"}, eou: 0.95}
	llm := &sequencedLLM{responses: []string{"Le leo: librería central, arroba gmail punto com."}}
	ms, _ := listenStream(t, stt, llm, fixedAudioTTS{})
	ms.Listen(&Listening{Patience: 900 * time.Millisecond, DoneWait: 120 * time.Millisecond,
		Done: func(s string) bool { return strings.Contains(s, "punto com") }})
	for i := 0; i < 4; i++ {
		piece(ms, 400*time.Millisecond)
		if n := atomic.LoadInt32(&llm.call); n != 0 {
			t.Fatalf("answered after piece %d", i+1)
		}
	}
	t0 := time.Now()
	piece(ms, 0)
	for atomic.LoadInt32(&llm.call) == 0 && time.Since(t0) < 2*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	if took := time.Since(t0); took > 600*time.Millisecond {
		t.Fatalf("a complete address waited %v", took)
	}
	time.Sleep(100 * time.Millisecond)
	if got := userTurns(ms); len(got) != 1 || got[0] != "librería central arroba a gmail punto com" {
		t.Fatalf("turns: %q", got)
	}
	if ms.listeningTo() != nil {
		t.Fatal("still listening after the agent spoke")
	}
}

// Nothing changes for a stream that is not listening: the same finished piece is answered at once.
func TestWithoutListeningAPieceIsAnsweredAtOnce(t *testing.T) {
	stt := &piecesSTT{pieces: []string{"librería"}, eou: 0.95}
	llm := &sequencedLLM{responses: []string{"¿Cómo dice?"}}
	ms, _ := listenStream(t, stt, llm, fixedAudioTTS{})
	piece(ms, 250*time.Millisecond)
	if atomic.LoadInt32(&llm.call) != 1 {
		t.Fatal("an ordinary turn waited")
	}
}

// A piece the recogniser returns nothing for is kept and read with the next one, not answered with "I didn't hear you".
func TestListeningKeepsAnUnheardPiece(t *testing.T) {
	stt := &piecesSTT{pieces: []string{"", "gmail"}, eou: 0.95}
	llm := &sequencedLLM{responses: []string{"Sí."}}
	tts := &recordingTTS{}
	ms, _ := listenStream(t, stt, llm, tts)
	ms.Listen(&Listening{Patience: 200 * time.Millisecond})
	piece(ms, 350*time.Millisecond)
	if n := atomic.LoadInt32(&llm.call); n != 0 || tts.all() != "" {
		t.Fatalf("answered an unheard piece: %d requests, spoke %q", n, tts.all())
	}
	piece(ms, 500*time.Millisecond)
	if atomic.LoadInt32(&llm.call) != 1 {
		t.Fatal("the next piece was not answered")
	}
}

// The tool that keeps listening ends the turn with nothing said: no filler, no "got it", no second request.
func TestSilentToolEndsTheTurnWithNothingSaid(t *testing.T) {
	a := &stepLLM{name: "a", call: "keep_listening"}
	tts := &recordingTTS{}
	ms, orch := listenStream(t, &MockSTTProvider{}, a, tts)
	orch.SetSilentTools("keep_listening")
	ms.session.SetTools([]Tool{toolNamed("keep_listening")})
	orch.RegisterTool("keep_listening", func(string) (string, error) {
		ms.Listen(&Listening{Patience: time.Second})
		return `{"ok":true}`, nil
	})
	ms.session.AddMessage("user", "librería")
	ms.mu.Lock()
	ms.payloadGen++
	gen := ms.payloadGen
	ms.mu.Unlock()
	ms.runStreamingLLM(context.Background(), a, gen, "librería")
	time.Sleep(200 * time.Millisecond)
	if tts.all() != "" {
		t.Fatalf("spoke %q", tts.all())
	}
	a.mu.Lock()
	asked := a.asked
	a.mu.Unlock()
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
	if ms.listeningTo() == nil || ms.getState() != StateIdle {
		t.Fatalf("listening %v, state %v", ms.listeningTo() != nil, ms.getState())
	}
	// and the exchange is in the conversation, so the model knows what it has heard
	msgs := ms.session.GetContextCopy()
	if msgs[len(msgs)-1].Role != "tool" {
		t.Fatalf("last message %+v", msgs[len(msgs)-1])
	}
}

// Saving a detail with it ends the turn the same way; a move to another step does not.
func TestEndsSilently(t *testing.T) {
	o := New(nil, nil, nil, DefaultConfig())
	o.SetSilentTools("keep_listening")
	o.SetFinalTools("save_details", "end_call")
	call := func(name, result string) toolExchange {
		return toolExchange{TC: ToolCallEventData{Name: name}, Result: result}
	}
	for _, c := range []struct {
		calls []toolExchange
		want  bool
	}{
		{[]toolExchange{call("keep_listening", `{"ok":true}`)}, true},
		{[]toolExchange{call("keep_listening", `{"ok":true}`), call("save_details", `{"ok":true}`)}, true},
		{[]toolExchange{call("keep_listening", `{"ok":true}`), call("save_details", `{"ok":true,"continue":true}`)}, false},
		{[]toolExchange{call("keep_listening", `{"ok":true}`), call("go_to_x", `{"ok":true,"continue":true}`)}, false},
		{[]toolExchange{call("keep_listening", `{"error":"not now"}`)}, false},
		{[]toolExchange{call("save_details", `{"ok":true}`)}, false},
		{nil, false},
	} {
		if got := o.endsSilently(c.calls); got != c.want {
			t.Errorf("%v: %v", c.calls, got)
		}
	}
}

// While listening the silence check-in waits its own time.
func TestListeningHoldsTheSilenceCheckIn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 80 * time.Millisecond
	cfg.FirstSpeaker = FirstSpeakerUser
	llm := &countingLLM{result: "¿Sigue ahí?"}
	orch := NewWithVAD(&MockSTTProvider{}, llm, fixedAudioTTS{}, NewRMSVAD(0.1, 100*time.Millisecond), cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("s"))
	defer ms.Close()
	ms.Listen(&Listening{Patience: time.Second, Silence: 700 * time.Millisecond})
	time.Sleep(450 * time.Millisecond)
	if n := llm.calls.Load(); n != 0 {
		t.Fatalf("checked in %d times on somebody who is dictating", n)
	}
	time.Sleep(700 * time.Millisecond)
	if n := llm.calls.Load(); n != 1 {
		t.Fatalf("%d check-ins after its own time", n)
	}
}

// Two check-ins when the host asks for two, then the host is told, once.
func TestSilenceCheckInsThenTheHost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 120 * time.Millisecond
	cfg.SilenceCheckIns = 2
	cfg.FirstSpeaker = FirstSpeakerUser
	var told atomic.Int32
	cfg.OnSilence = func() { told.Add(1) }
	llm := &countingLLM{result: "¿Sigue ahí?"}
	orch := NewWithVAD(&MockSTTProvider{}, llm, fixedAudioTTS{}, NewRMSVAD(0.1, 100*time.Millisecond), cfg)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("s"))
	defer ms.Close()
	go func() {
		for range ms.Events() {
		}
	}()
	time.Sleep(2500 * time.Millisecond)
	if n, h := llm.calls.Load(), told.Load(); n != 2 || h != 1 {
		t.Fatalf("%d check-ins, host told %d times", n, h)
	}
}

// With an opening wait the agent opens only when the other person says nothing.
func TestOpeningWaitsForTheOtherPerson(t *testing.T) {
	run := func(speaks bool) []string {
		cfg := DefaultConfig()
		cfg.SilenceTimeout = 0
		cfg.SpeculativeLLM = false
		cfg.OpeningWait = 400 * time.Millisecond
		cfg.OpeningMessage = "¿Hablo con el restaurante?"
		cfg.SampleRate = 16000
		tts := &recordingTTS{}
		orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "Panadería Rosa, dígame"}, &sequencedLLM{responses: []string{"Le llamaba por unos tickets."}}, tts, nil, cfg, &NoOpLogger{})
		ms := orch.NewManagedStream(context.Background(), NewConversationSession("open"))
		defer ms.Close()
		go func() {
			for range ms.Events() {
			}
		}()
		ms.NotifyTransportReady()
		time.Sleep(150 * time.Millisecond)
		if got := tts.all(); got != "" {
			t.Fatalf("spoke before the wait: %q", got)
		}
		if speaks {
			piece(ms, 0)
		}
		time.Sleep(900 * time.Millisecond)
		return strings.Split(tts.all(), "|")
	}
	if got := run(false); len(got) != 1 || got[0] != "¿Hablo con el restaurante?" {
		t.Fatalf("nobody spoke: %q", got)
	}
	if got := run(true); len(got) != 1 || got[0] != "Le llamaba por unos tickets." {
		t.Fatalf("they spoke first: %q", got)
	}
}

// fakeJudge says more until the words hold the end.
type fakeJudge struct {
	active atomic.Bool
	asked  atomic.Int32
	slow   time.Duration
}

func (f *fakeJudge) Active() bool { return f.active.Load() }
func (f *fakeJudge) Listening() *Listening {
	return &Listening{Patience: 900 * time.Millisecond, DoneWait: 100 * time.Millisecond}
}
func (f *fakeJudge) Judge(ctx context.Context, t DictationTurn) DictationTurnKind {
	f.asked.Add(1)
	select {
	case <-time.After(f.slow):
	case <-ctx.Done():
		return ""
	}
	switch {
	case strings.Contains(t.LastPiece, "lo tiene"):
		return DictationOther
	case strings.Contains(t.Words, "punto com"):
		return DictationWhole
	case strings.Contains(t.Words, "librería"):
		return DictationMore
	}
	return DictationOther
}

// The judge turns listening on by itself: the pieces never reach the model one by one, the whole does soon after it is said.
func TestDictationJudgeHoldsThePieces(t *testing.T) {
	stt := &piecesSTT{pieces: []string{"librería", "central", "arroba gmail", "punto com"}, eou: 0.95}
	llm := &sequencedLLM{responses: []string{"Le leo."}}
	ms, orch := listenStream(t, stt, llm, fixedAudioTTS{})
	j := &fakeJudge{}
	j.active.Store(true)
	orch.SetDictationJudge(j)
	for i := 0; i < 3; i++ {
		piece(ms, 400*time.Millisecond)
		if n := atomic.LoadInt32(&llm.call); n != 0 {
			t.Fatalf("answered after piece %d", i+1)
		}
	}
	piece(ms, 500*time.Millisecond)
	if got := userTurns(ms); atomic.LoadInt32(&llm.call) != 1 || len(got) != 1 || got[0] != "librería central arroba gmail punto com" {
		t.Fatalf("asked %d times, turns %q", atomic.LoadInt32(&llm.call), got)
	}
}

// What the caller says to the agent in the middle of it is answered at once, not after the patience.
func TestDictationJudgeAnswersAQuestionInTheMiddle(t *testing.T) {
	stt := &piecesSTT{pieces: []string{"librería", "lo tiene"}, eou: 0.95}
	llm := &sequencedLLM{responses: []string{"Sí, dígame."}}
	ms, orch := listenStream(t, stt, llm, fixedAudioTTS{})
	j := &fakeJudge{}
	j.active.Store(true)
	orch.SetDictationJudge(j)
	piece(ms, 300*time.Millisecond)
	piece(ms, 350*time.Millisecond)
	if atomic.LoadInt32(&llm.call) != 1 {
		t.Fatal("a question in the middle was not answered")
	}
}

// An ordinary turn is answered as soon as the judge says so; one it has no time for is an ordinary turn; and a judge that
// is not active is never asked.
func TestDictationJudgeLeavesOrdinaryTurns(t *testing.T) {
	for _, c := range []struct {
		name   string
		active bool
		slow   time.Duration
		within time.Duration
		asked  bool
	}{{"other", true, 0, 300 * time.Millisecond, true}, {"no answer in time", true, 5 * time.Second, dictationJudgeBudget + 400*time.Millisecond, true}, {"not active", false, 0, 200 * time.Millisecond, false}} {
		t.Run(c.name, func(t *testing.T) {
			llm := &sequencedLLM{responses: []string{"Claro."}}
			ms, orch := listenStream(t, &piecesSTT{pieces: []string{"no me interesa"}, eou: 0.95}, llm, fixedAudioTTS{})
			j := &fakeJudge{slow: c.slow}
			j.active.Store(c.active)
			orch.SetDictationJudge(j)
			t0 := time.Now()
			piece(ms, 0)
			for atomic.LoadInt32(&llm.call) == 0 && time.Since(t0) < 3*time.Second {
				time.Sleep(5 * time.Millisecond)
			}
			if took := time.Since(t0); took > c.within+60*time.Millisecond {
				t.Fatalf("answered after %v", took)
			}
			if (j.asked.Load() > 0) != c.asked {
				t.Fatalf("asked %d times", j.asked.Load())
			}
		})
	}
}

// narratingLLM says what it is about to do, then moves; the next step speaks.
type narratingLLM struct {
	stepLLM
	words []string
}

func (s *narratingLLM) StreamComplete(ctx context.Context, m []Message, t []Tool, onChunk func(string) error, onToolCall func(ToolCallEventData) error) (string, error) {
	for _, w := range s.words {
		if err := onChunk(w); err != nil {
			return "", err
		}
	}
	return strings.Join(s.words, ""), onToolCall(ToolCallEventData{Name: s.call, Arguments: "{}", CallID: "c1"})
}

// What is written with a move is not said, nor kept in the conversation; with any other tool it is, as before.
func TestWordsWithAMoveAreNotSaid(t *testing.T) {
	run := func(tool string) (spoken string, kept string) {
		a := &narratingLLM{stepLLM: stepLLM{name: "a", call: tool}, words: []string{"It's a match, ", "so I move on."}}
		b := &stepLLM{name: "b", say: "Le llamaba por unos tickets."}
		tts := &recordingTTS{}
		ms, orch := listenStream(t, &MockSTTProvider{}, a, tts)
		orch.SetMoveTools("go_to_motivo")
		orch.SetQuietTools("go_to_motivo", "look_up")
		ms.session.SetTools([]Tool{toolNamed("go_to_motivo"), toolNamed("look_up")})
		h := func(string) (string, error) { orch.SetLLMProvider(b); return `{"ok":true,"continue":true}`, nil }
		orch.RegisterTool("go_to_motivo", h)
		orch.RegisterTool("look_up", h)
		ms.session.AddMessage("user", "Ferretería Sol")
		ms.mu.Lock()
		ms.payloadGen++
		gen := ms.payloadGen
		ms.mu.Unlock()
		ms.runStreamingLLM(context.Background(), a, gen, "Ferretería Sol")
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && !strings.Contains(tts.all(), "tickets"); {
			time.Sleep(20 * time.Millisecond)
		}
		for _, m := range ms.session.GetContextCopy() {
			kept += m.Content + "|"
		}
		return tts.all(), kept
	}
	if spoken, kept := run("go_to_motivo"); spoken != "Le llamaba por unos tickets." || strings.Contains(kept, "match") {
		t.Fatalf("spoken %q kept %q", spoken, kept)
	}
	if spoken, _ := run("look_up"); !strings.Contains(spoken, "It's a match, so I move on.") {
		t.Fatalf("an ordinary tool's words were not said: %q", spoken)
	}
}
