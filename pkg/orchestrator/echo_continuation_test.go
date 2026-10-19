package orchestrator

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// confirmingVAD is an RMSVAD that says, as SileroVAD does, how much speech it hears before it
// reports an onset.
type confirmingVAD struct {
	*RMSVAD
	confirm time.Duration
}

func (v confirmingVAD) SpeechConfirmDuration() time.Duration { return v.confirm }
func (v confirmingVAD) Clone() VADProvider {
	return confirmingVAD{RMSVAD: v.RMSVAD.Clone().(*RMSVAD), confirm: v.confirm}
}

// webVAD is the web agent's: Silero with 6 frames of 32 ms to confirm speech (pkg/api voice_agent.go).
func webVAD() VADProvider {
	return confirmingVAD{RMSVAD: NewRMSVAD(0.05, 50*time.Millisecond), confirm: 192 * time.Millisecond}
}

// syllables is a speech-like loudness contour, one value per 20 ms echo frame: about four syllables
// a second.
func syllables(n int, phase float64) []float64 {
	env := make([]float64, n)
	for i := range env {
		env[i] = 0.2 + 0.8*math.Abs(math.Sin(float64(i)*0.26+phase))*(0.6+0.4*math.Abs(math.Cos(float64(i)*0.07+phase)))
	}
	return env
}

// voiced is 16 kHz PCM whose loudness follows env (one value per 20 ms frame) on a tone of freq.
func voiced(env []float64, freq float64) []byte {
	per := echoFrameBytes / 2
	out := make([]byte, 0, len(env)*echoFrameBytes)
	for f, a := range env {
		for i := 0; i < per; i++ {
			v := int16(a * 12000 * math.Sin(float64(f*per+i)*freq))
			out = append(out, byte(v), byte(v>>8))
		}
	}
	return out
}

// continuationOver is the caller's second half on 2026-10-19 as the echo check saw it: speech from
// the onset on, loudest where it lines up with the agent's first sentence lag frames later — the
// envelopes match as well as an echo's would — quieter around it, then the VAD's hangover.
func continuationOver(agent []float64, lag, n int) []float64 {
	c := make([]float64, n)
	for i := range c {
		switch j := i - lag; {
		case j >= 0 && j < len(agent):
			c[i] = 0.5 * agent[j]
		case i < lag+len(agent)+8:
			c[i] = 0.06
		default:
			c[i] = 0.005
		}
	}
	return c
}

// The 2026-10-19 Godot session (agent_1792403239900718816): the reply to the caller's first half
// began playing 9 ms before the VAD confirmed their second half, which it had been hearing for
// 192 ms. The two envelopes correlated (0.776 there; higher here) only because two voices started
// together. Speech that began while the agent was silent is not its echo, however well they match;
// the agent's own voice, audible before the onset, still is.
func TestEchoCannotPrecedeItsSource(t *testing.T) {
	orch := NewWithVAD(&MockSTTProvider{}, &MockLLMProvider{}, &MockTTSProvider{}, webVAD(), DefaultConfig())
	stream := orch.NewManagedStream(context.Background(), NewConversationSession("echo-source"))
	defer stream.Close()
	if got := stream.vadOnsetFrames(); got != 10 {
		t.Fatalf("vadOnsetFrames = %d, want 10 (192 ms of 20 ms frames)", got)
	}

	// Far end: 3 s of nothing, then the first sentence from 10 ms before the onset (420 ms, cut), then
	// nothing again until the end of the near end. Near end: the caller from the onset, 860 ms.
	agent := syllables(21, 0.4)
	caller := continuationOver(agent, 8, 43)
	onset := 3 * 16000 * 2
	far := make([]byte, onset+len(caller)*echoFrameBytes)
	copy(far[onset-320:], voiced(agent, 0.37))
	near := voiced(caller, 0.21)

	raw, _ := correlateEnvelopes(rmsEnvelope(near, echoFrameBytes), rmsEnvelope(far, echoFrameBytes)[150-echoMaxLagFrames:])
	if raw < echoCorrelationThreshold {
		t.Fatalf("the reconstruction does not reproduce the false match: raw score %.3f", raw)
	}
	stream.echoMu.Lock()
	stream.echoFarEndBuf, stream.echoNearEndBuf, stream.echoNearEndGen = far, near, 1
	stream.echoMu.Unlock()
	r := stream.acousticEchoCheck(1)
	if r.echo {
		t.Fatalf("speech begun before the agent was audible was taken for its echo (score %.3f, lag %d ms)", r.score, r.lagMs)
	}
	if r.unsourcedScore < echoCorrelationThreshold {
		t.Fatalf("unsourced score %.3f: the log would not show what the onset rule set aside", r.unsourcedScore)
	}

	// The agent's own voice: it had been talking for 2 s when the microphone heard it, 160 ms late.
	talk := voiced(syllables(100, 1.1), 0.37)
	far = append(make([]byte, 16000*2), talk...)
	lagBytes, nearBytes := 8*echoFrameBytes, 43*echoFrameBytes
	near = attenuate(far[len(far)-lagBytes-nearBytes:len(far)-lagBytes], 0.3)
	stream.echoMu.Lock()
	stream.echoFarEndBuf, stream.echoNearEndBuf, stream.echoNearEndGen = far, near, 2
	stream.echoMu.Unlock()
	if r := stream.acousticEchoCheck(2); !r.echo || r.lagMs != 160 {
		t.Fatalf("a real echo was missed: echo=%v score %.3f lag %d ms", r.echo, r.score, r.lagMs)
	}
}

// scriptedTTS says the first sentence it is given as first, then holds until hold is closed (the
// caller's VAD onset lands while it is still synthesising, as on 2026-10-19); later sentences are
// later, else a second of tone.
type scriptedTTS struct {
	mu    sync.Mutex
	calls int
	first []byte
	later [][]byte
	hold  chan struct{}
	held  chan struct{} // closed once the first sentence is all out and the hold begins
}

func (s *scriptedTTS) pcm() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	switch {
	case s.calls == 1:
		return s.first, s.hold != nil
	case s.calls-2 < len(s.later):
		return s.later[s.calls-2], false
	}
	return tone(16000, 0.3, 6000), false
}

func (s *scriptedTTS) Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error) {
	pcm, _ := s.pcm()
	return pcm, nil
}

func (s *scriptedTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	pcm, hold := s.pcm()
	for i := 0; i < len(pcm); i += 3200 {
		end := i + 3200
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := onChunk(pcm[i:end]); err != nil {
			return err
		}
	}
	if hold {
		close(s.held)
		select {
		case <-s.hold:
		case <-ctx.Done():
		}
	}
	return nil
}
func (s *scriptedTTS) Abort() error { return nil }
func (s *scriptedTTS) Name() string { return "scriptedTTS" }

// lengthSTT transcribes by audio length: each utterance in a test has its own.
type lengthSTT struct {
	mu    sync.Mutex
	byLen map[int]string
}

func (s *lengthSTT) Transcribe(ctx context.Context, audio []byte, lang Language) (TranscriptionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return TranscriptionResult{Text: s.byLen[len(audio)]}, nil
}
func (s *lengthSTT) Name() string { return "lengthSTT" }

func waitUntil(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (l *recordingLogger) has(prefix, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (l *recordingLogger) dump() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func lastUserMessage(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

const (
	bromFirstHalf = "Hey Brom! I'm heading up the mountain pass tonight."
	bromSecond    = "What should I take?"
	bromMerged    = "Hey Brom! I'm heading up the mountain pass tonight. What should I take?"
	bromReply1    = "Bring your warmest cloak."
	bromReply2    = "The pass gets icy after dark, so pack a lantern too."
	bromAnswer    = "A cloak, a lantern and dry boots."
)

// replayBromContinuation replays agent_1792403239900718816, 2026-10-19 09:47:28-31 UTC, through the
// stream: a web caller (play-through transport) says "Hey Brom! I'm heading up the mountain pass
// tonight." and, after a pause, "What should I take?". The first half is answered; the reply's first
// sentence starts playing as the VAD confirms the second half, its second sentence is discarded
// before it plays, the second half is fed to the echo check frame by frame as handleAudio does, and
// the VAD ends it. It returns the stream and the model, with the second half's turn in flight.
func replayBromContinuation(t *testing.T, vad VADProvider, log *recordingLogger) (*ManagedStream, *chunkedLLM) {
	t.Helper()
	agent := syllables(21, 0.4) // the first sentence: 420 ms
	caller := continuationOver(agent, 8, 43)
	audio1 := make([]byte, 2570*16) // 2570 ms at 16 kHz
	audio2 := voiced(caller, 0.21)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 16000
	llm := &chunkedLLM{script: [][]string{{bromReply1 + " ", bromReply2}, {bromAnswer}}}
	tts := &scriptedTTS{first: voiced(agent, 0.37), hold: make(chan struct{}), held: make(chan struct{})}
	stt := &lengthSTT{}
	orch := NewWithAllLayers(stt, llm, tts, vad, cfg, log)
	session := NewConversationSession("brom")
	session.CurrentLanguage = LanguageEn
	stream := orch.NewManagedStream(context.Background(), session)
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	stream.SetTransportPlaysThroughBargeIn(true)
	stt.byLen = map[int]string{
		len(audio1): bromFirstHalf,
		len(audio2): bromSecond,
		len(joinUtteranceAudio(audio1, audio2, int(stream.inputSampleRate))): bromMerged,
	}

	stream.advanceFarEndEcho(time.Now().Add(-3 * time.Second)) // the caller's speaker: silent for the last 3 s

	// 09:47:29.818 the VAD ends the first half; it is answered.
	stream.mu.Lock()
	stream.state, stream.utteranceSeq, stream.inflightUtterances = StateProcessing, 1, 1
	stream.mu.Unlock()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		stream.processUtterance(audio1, 2570*time.Millisecond, 1)
	}()
	select {
	case <-tts.held:
	case <-time.After(3 * time.Second):
		t.Fatalf("the reply's first sentence did not go out:\n%s", log.dump())
	}

	// 09:47:30.260: it is playing (all 420 ms of it sent) as the VAD confirms the second half, 192 ms of
	// which it has heard.
	stream.mu.Lock()
	stream.vadSpeaking = true
	state := stream.state
	stream.mu.Unlock()
	stream.onVADStart(state)
	onsetAt := time.Now()
	close(tts.hold) // the second sentence reaches speakText with the caller speaking: discarded
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the first half's turn did not finish")
	}

	// The second half, 43 frames, as handleAudio feeds them: the speaker's clock, the microphone, and
	// the play-through stop. Time is given, not slept, so the frames line up with the far end exactly.
	for f := range caller {
		now := onsetAt.Add(time.Duration(f+1) * 20 * time.Millisecond)
		stream.advanceFarEndEcho(now)
		stream.noteNearEndEcho(audio2[f*echoFrameBytes:(f+1)*echoFrameBytes], 1)
		stream.stopPlayThroughIfDue(now)
	}
	time.Sleep(time.Until(onsetAt.Add(time.Duration(len(caller)) * 20 * time.Millisecond)))

	// 09:47:31.283 the VAD ends the second half.
	stream.mu.Lock()
	stream.vadSpeaking = false
	stream.userAudio = audio2
	state = stream.state
	stream.mu.Unlock()
	stream.onVADEnd(state)
	return stream, llm
}

func expectBromAnswered(t *testing.T, stream *ManagedStream, llm *chunkedLLM) {
	t.Helper()
	ev := waitForEventType(t, stream, TranscriptRevised, 3*time.Second)
	if rt, _ := ev.Data.(RevisedTranscript); rt.Previous != bromFirstHalf || rt.Text != bromMerged {
		t.Fatalf("TranscriptRevised = %+v, want the two halves as one turn", ev.Data)
	}
	waitUntil(t, "the joined turn to reach the model", 3*time.Second, func() bool {
		llm.mu.Lock()
		defer llm.mu.Unlock()
		return len(llm.requests) == 2
	})
	llm.mu.Lock()
	asked := lastUserMessage(llm.requests[1])
	llm.mu.Unlock()
	if asked != bromMerged {
		t.Fatalf("the model was asked %q, want %q", asked, bromMerged)
	}
	ev = waitForEventType(t, stream, BotResponse, 3*time.Second)
	if ev.Data != bromAnswer {
		t.Fatalf("BotResponse = %v, want the answer to the joined turn", ev.Data)
	}
}

// The sequence itself: the second half began before any of the reply played, so it is not an echo;
// the caller heard 400 ms of a reply to half a question, so the halves are one turn, and answered.
// It used to be vetoed as an echo (acoustic 0.776), "resumed" a reply that had nothing left to play,
// and the caller heard nothing until the silence check-in 11 s later.
func TestEchoVeto_BromContinuationIsAnswered(t *testing.T) {
	log := &recordingLogger{}
	stream, llm := replayBromContinuation(t, webVAD(), log)
	expectBromAnswered(t, stream, llm)
	if !log.has("INFO Discarding generated response", "") {
		t.Fatalf("the replay did not discard the reply's second sentence before it played")
	}
	if !log.has("INFO Caller kept talking over the agent: stopping playback", "") {
		t.Fatalf("speech the agent did not produce must stop playback")
	}
	if log.count("INFO Barge-in looks like an echo") != 0 {
		t.Fatalf("the second half was taken for an echo")
	}
}

// The same sequence where the VAD does not say how long it listens before an onset, so nothing rules
// the match out: the veto fires, finds nothing of the reply left to play, and does not drop the turn.
// The caller had heard next to none of the reply: their words are joined to their first half.
func TestEchoVeto_NothingToResumeJoinsTheContinuation(t *testing.T) {
	log := &recordingLogger{}
	stream, llm := replayBromContinuation(t, NewRMSVAD(0.05, 50*time.Millisecond), log)
	expectBromAnswered(t, stream, llm)
	if !log.has("INFO Barge-in looks like an echo of the bot's own speech", "joined_to_previous") {
		t.Fatalf("the veto's outcome was not logged as joined_to_previous:\n%s", log.dump())
	}
}

// After the playout, for echoAfterHeard: what the play-through transport did.
const (
	transportStopped   = iota // told to stop heardMs in (its own check had not matched yet)
	transportPlaying          // still playing the reply when the utterance ends
	transportPlayedOut        // played the whole reply before the utterance ended
)

// echoAfterHeard sets up the echo check after a two-sentence reply (first 2 s, second 1 s) to the
// caller's first utterance: their microphone picked up the agent's own voice heardMs into the reply,
// and the transport then did what after says. The microphone heard an attenuated copy of what had
// played, 160 ms late, transcribed as said; the VAD ends the utterance and its turn is in flight.
func echoAfterHeard(t *testing.T, heardMs, after int, said string, log *recordingLogger) (*ManagedStream, *chunkedLLM) {
	t.Helper()
	first := voiced(syllables(100, 0.9), 0.37)
	second := voiced(syllables(50, 2.0), 0.37)
	audio1 := make([]byte, 1800*16)

	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 16000
	llm := &chunkedLLM{script: [][]string{{bromReply1 + " ", bromReply2}, {bromAnswer}}}
	stt := &lengthSTT{byLen: map[int]string{len(audio1): bromFirstHalf}}
	orch := NewWithAllLayers(stt, llm, &scriptedTTS{first: first, later: [][]byte{second}}, webVAD(), cfg, log)
	session := NewConversationSession("echo-after-heard")
	session.CurrentLanguage = LanguageEn
	stream := orch.NewManagedStream(context.Background(), session)
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	stream.SetTransportPlaysThroughBargeIn(true)

	stream.mu.Lock()
	stream.state, stream.utteranceSeq, stream.inflightUtterances = StateProcessing, 1, 1
	stream.mu.Unlock()
	stream.processUtterance(audio1, 1800*time.Millisecond, 1)
	var start time.Time
	stream.mu.Lock()
	if len(stream.playout.segs) > 0 && len(stream.playout.segs[0].intervals) > 0 {
		start = stream.playout.segs[0].intervals[0].start
	}
	stream.vadSpeaking = true
	state := stream.state
	stream.mu.Unlock()
	if start.IsZero() {
		t.Fatalf("the reply to the first utterance did not play:\n%s", log.dump())
	}

	heard := start.Add(time.Duration(heardMs) * time.Millisecond)
	time.Sleep(time.Until(heard.Add(-450 * time.Millisecond)))
	stream.onVADStart(state)
	switch after {
	case transportStopped:
		stream.stopPlayThroughIfDue(heard) // nothing in the near end yet: not an echo, so it stops
	case transportPlayedOut:
		stream.mu.Lock()
		end := stream.playout.end
		stream.mu.Unlock()
		time.Sleep(time.Until(end.Add(20 * time.Millisecond)))
	}

	// What had played, from 1 s of silence before it to 300 ms past the moment heard, and the
	// microphone's copy of its last 600 ms, 160 ms late.
	played := first
	if n := heardMs * 32; n < len(played) {
		played = played[:n]
	}
	far := append(make([]byte, 16000*2), played...)
	far = append(far, make([]byte, 300*32)...)
	lagBytes, nearBytes := 8*echoFrameBytes, 30*echoFrameBytes
	near := attenuate(far[len(far)-lagBytes-nearBytes:len(far)-lagBytes], 0.3)
	stt.mu.Lock()
	stt.byLen[len(near)] = said
	stt.mu.Unlock()
	gen := stream.payloadGen
	stream.echoMu.Lock()
	stream.echoFarEndBuf, stream.echoNearEndBuf, stream.echoNearEndGen = far, near, gen
	stream.echoMu.Unlock()

	drainUntil(stream, 20*time.Millisecond)
	stream.userSpeakingSince = time.Now().Add(-time.Duration(len(near)/32) * time.Millisecond)
	stream.mu.Lock()
	stream.vadSpeaking = false
	stream.userAudio = near
	state = stream.state
	stream.mu.Unlock()
	stream.onVADEnd(state)
	return stream, llm
}

// The agent's own words came back after the caller had heard 1.6 s of the reply, and the transport
// had already been told to stop: the rest of the reply is said again, from the start of the sentence
// that was cut, with no model call. It used to be lost, and the caller heard nothing until the
// silence check-in.
func TestEchoVeto_NothingToResumeSaysTheRestAgain(t *testing.T) {
	log := &recordingLogger{}
	stream, llm := echoAfterHeard(t, 1600, transportStopped, "Bring your warmest cloak.", log)
	waitUntil(t, "the echo check", 3*time.Second, func() bool {
		return log.has("INFO Barge-in looks like an echo of the bot's own speech", "")
	})
	ev := waitForEventType(t, stream, BotResponse, 3*time.Second)
	if want := bromReply1 + " " + bromReply2; ev.Data != want {
		t.Fatalf("BotResponse = %q, want the reply again from the cut sentence: %q", ev.Data, want)
	}
	waitForEventType(t, stream, AudioChunk, 3*time.Second)
	if !log.has("INFO Barge-in looks like an echo of the bot's own speech", "said_again") {
		t.Fatalf("the veto's outcome was not logged as said_again:\n%s", log.dump())
	}
	llm.mu.Lock()
	calls := len(llm.requests)
	llm.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the model was asked %d times; saying the reply again needs no model call", calls)
	}
	ctx := stream.session.GetContextCopy()
	var tail []string
	for _, m := range ctx {
		if m.Role != "system" {
			tail = append(tail, m.Role+": "+m.Content)
		}
	}
	if len(tail) != 3 || tail[0] != "user: "+bromFirstHalf || !strings.HasSuffix(tail[1], interruptionMark) ||
		tail[2] != "assistant: "+bromReply1+" "+bromReply2 {
		t.Fatalf("context = %q, want the caller's turn, what they heard of the reply, and the reply said again", tail)
	}
}

// A veto whose reply is still playing on the transport resumes it, as before.
func TestEchoVeto_ReplyStillPlayingIsResumed(t *testing.T) {
	log := &recordingLogger{}
	stream, llm := echoAfterHeard(t, 1600, transportPlaying, "Bring your warmest cloak.", log)
	ev := waitForEventType(t, stream, BotResumed, 3*time.Second)
	if ev.Data != "speaking" {
		t.Fatalf("BotResumed = %v", ev.Data)
	}
	if !log.has("INFO Barge-in looks like an echo of the bot's own speech", "resumed") {
		t.Fatalf("the veto's outcome was not logged as resumed:\n%s", log.dump())
	}
	if seen := drainUntil(stream, 300*time.Millisecond); seen[BotResponse].Type != "" || seen[TranscriptFinal].Type != "" {
		t.Fatalf("a resumed reply must not be followed by a new one or a new turn (seen %v)", seen)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.requests) != 1 {
		t.Fatalf("the model was asked %d times", len(llm.requests))
	}
}

// The agent's echo of a reply the caller had heard to the end costs them nothing: it is dropped, and
// nothing is said again.
func TestEchoVeto_NothingLostIsDropped(t *testing.T) {
	log := &recordingLogger{}
	stream, llm := echoAfterHeard(t, 1600, transportPlayedOut, "Bring your warmest cloak.", log)
	waitUntil(t, "the echo check", 3*time.Second, func() bool {
		return log.has("INFO Barge-in looks like an echo of the bot's own speech", "")
	})
	if !log.has("INFO Barge-in looks like an echo of the bot's own speech", "nothing_lost") {
		t.Fatalf("the veto's outcome was not logged as nothing_lost:\n%s", log.dump())
	}
	if seen := drainUntil(stream, 300*time.Millisecond); seen[BotResponse].Type != "" || seen[TranscriptFinal].Type != "" {
		t.Fatalf("an echo of a reply heard in full started something (seen %v)", seen)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.requests) != 1 {
		t.Fatalf("the model was asked %d times", len(llm.requests))
	}
}
