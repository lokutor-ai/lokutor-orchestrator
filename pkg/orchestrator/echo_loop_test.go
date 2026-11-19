package orchestrator

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The 2026-11-17 call ed25f07d, replayed: the agent's own reply comes back through the caller's microphone over and
// over, the transport is told to stop each time before the acoustic check matches it, and the transcript then shows
// it was the agent's own words. The lost reply used to be said again from the start of its cut sentence (the
// greeting), so the same echo cut it again: six greetings and the call never got past them.

const loopEcho = "Bring your warmest" // what the recogniser heard of the agent's own voice, every round

// echoRound plays the current reply for heardMs, then the agent's echo arrives: the transport is told to stop
// (nothing matched yet, as in ed25f07d), the microphone holds an attenuated copy of the agent's audio, transcribed as
// loopEcho, and the utterance ends.
func echoRound(t *testing.T, stream *ManagedStream, stt *lengthSTT, heardMs int, log *recordingLogger) {
	echoRoundSaying(t, stream, stt, heardMs, loopEcho, log)
}

// echoRoundSaying is echoRound with the recogniser hearing said.
func echoRoundSaying(t *testing.T, stream *ManagedStream, stt *lengthSTT, heardMs int, said string, log *recordingLogger) {
	t.Helper()
	start := playoutStart(t, stream, log)
	time.Sleep(time.Until(start.Add(time.Duration(heardMs-450) * time.Millisecond)))
	stream.mu.Lock()
	stream.vadSpeaking = true
	// The replay is still playing on the caller's side (the test's synthesis finishes at once; production's
	// stream is speaking while it plays).
	stream.state = StateSpeaking
	state := stream.state
	stream.mu.Unlock()
	stream.onVADStart(state)
	stream.mu.Lock()
	stopAt := stream.onset.at.Add(450 * time.Millisecond) // the caller's microphone has carried it past the 400 ms
	stream.mu.Unlock()
	stream.stopPlayThroughIfDue(stopAt)
	far, near := echoBuffers(0)
	stt.mu.Lock()
	stt.byLen[len(near)] = said
	stt.mu.Unlock()
	endEchoUtterance(stream, far, near)
}

// echoLoopStream is echoAfterHeard's first reply with replays four seconds long, cut by the agent's own echo 450 ms
// in, before its first word was out (ed25f07d: 0.84 s of a 9.5 s greeting).
func echoLoopStream(t *testing.T, log *recordingLogger, reply1 string, cutMs int, echoSaid string) *ManagedStream {
	t.Helper()
	first := voiced(syllables(200, 0.9), 0.37) // 4 s
	var later [][]byte
	for i := 0; i < 8; i++ {
		later = append(later, voiced(syllables(200, 0.9), 0.37))
	}
	audio1 := make([]byte, 1800*16)
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 0
	cfg.SampleRate = 16000
	llm := &chunkedLLM{script: [][]string{{reply1 + " ", bromReply2}, {bromAnswer}}}
	stt := &lengthSTT{byLen: map[int]string{len(audio1): bromFirstHalf}}
	orch := NewWithAllLayers(stt, llm, &scriptedTTS{first: first, later: later}, webVAD(), cfg, log)
	session := NewConversationSession("echo-loop")
	session.CurrentLanguage = LanguageEn
	stream := orch.NewManagedStream(context.Background(), session)
	t.Cleanup(stream.Close)
	stream.SetPlaybackRate(16000)
	stream.SetTransportPlaysThroughBargeIn(true)
	stream.mu.Lock()
	stream.state, stream.utteranceSeq, stream.inflightUtterances = StateProcessing, 1, 1
	stream.mu.Unlock()
	stream.processUtterance(audio1, 1800*time.Millisecond, 1)
	echoRoundSaying(t, stream, stt, cutMs, echoSaid, log)
	return stream
}

func playoutStart(t *testing.T, stream *ManagedStream, log *recordingLogger) time.Time {
	t.Helper()
	var start time.Time
	waitUntil(t, "the reply to play", 3*time.Second, func() bool {
		stream.mu.Lock()
		defer stream.mu.Unlock()
		if stream.playout.gen == stream.payloadGen && len(stream.playout.segs) > 0 && len(stream.playout.segs[0].intervals) > 0 {
			start = stream.playout.segs[0].intervals[0].start
			return true
		}
		return false
	})
	return start
}

// echoBuffers is a far end (the agent's audio after 1 s of silence) and a near end: its last 600 ms, 160 ms late,
// attenuated, with mix of an unrelated voice added (0: the echo alone; 1: the other voice alone).
func echoBuffers(mix float64) (far, near []byte) {
	played := voiced(syllables(60, 0.9), 0.37)
	far = append(make([]byte, 16000*2), played...)
	far = append(far, make([]byte, 300*32)...)
	lagBytes, nearBytes := 8*echoFrameBytes, 30*echoFrameBytes
	echo := attenuate(far[len(far)-lagBytes-nearBytes:len(far)-lagBytes], 0.3)
	other := voiced(randomEnvelope(60, 7), 0.23)
	near = make([]byte, nearBytes)
	for i := 0; i+1 < nearBytes; i += 2 {
		e := float64(int16(uint16(echo[i]) | uint16(echo[i+1])<<8))
		o := 0.0
		if i+1 < len(other) {
			o = float64(int16(uint16(other[i]) | uint16(other[i+1])<<8))
		}
		v := int16(math.Max(-32768, math.Min(32767, (1-mix)*e+mix*o)))
		near[i], near[i+1] = byte(v), byte(uint16(v)>>8)
	}
	return far, near
}

// randomEnvelope is a speech-like loudness contour with nothing in common with syllables'.
func randomEnvelope(n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	env := make([]float64, n*8)
	for i := range env {
		env[i] = 0.2 + 0.8*r.Float64()
	}
	return env
}

func setEchoBuffers(stream *ManagedStream, far, near []byte) {
	stream.mu.Lock()
	gen := stream.payloadGen
	stream.mu.Unlock()
	stream.echoMu.Lock()
	stream.echoFarEndBuf, stream.echoNearEndBuf, stream.echoNearEndGen = far, near, gen
	stream.echoMu.Unlock()
}

func endEchoUtterance(stream *ManagedStream, far, near []byte) {
	setEchoBuffers(stream, far, near)
	drainUntil(stream, 20*time.Millisecond)
	stream.userSpeakingSince = time.Now().Add(-time.Duration(len(near)/32) * time.Millisecond)
	stream.mu.Lock()
	stream.vadSpeaking = false
	stream.userAudio = near
	state := stream.state
	stream.mu.Unlock()
	stream.onVADEnd(state)
}

// loopToProtected runs ed25f07d's first rounds: the reply, cut by its own echo, said again and cut by the same echo
// again, until the next replay is protected. It returns what the agent said each time.
func loopToProtected(t *testing.T, log *recordingLogger) (*ManagedStream, *lengthSTT, []string) {
	t.Helper()
	stream := echoLoopStream(t, log, bromReply1, 450, loopEcho)
	stt := stream.orch.stt.(*lengthSTT)
	var said []string
	said = append(said, bromReply1+" "+bromReply2)
	ev := waitForEventType(t, stream, BotResponse, 3*time.Second) // round 1's replay
	said = append(said, ev.Data.(string))
	echoRound(t, stream, stt, 450, log) // the same echo cuts the replay before a word of it is out
	waitUntil(t, "the second echo verdict", 3*time.Second, func() bool {
		return strings.Count(log.dump(), "Barge-in looks like an echo") >= 2
	})
	ev = waitForEventType(t, stream, BotResponse, 3*time.Second)
	said = append(said, ev.Data.(string))
	waitUntil(t, "the protected replay", 3*time.Second, func() bool {
		return log.has("INFO echo loop: replay protected", "echo_confirmed")
	})
	return stream, stt, said
}

// ed25f07d: a long greeting, its echo cutting it 0.85 s in each time. Every replay starts at the word that was cut,
// so only the first one starts with the greeting, each is shorter than the one before, and the loop runs out.
func TestEchoLoop_Ed25f07dEndsWithOneGreeting(t *testing.T) {
	log := &recordingLogger{}
	greeting := "Hello Samuel, this is Sergio from Sunny Warehouses, calling about the warehouse assistant job you " +
		"applied for last week, and just so you know I am a virtual assistant and this call is recorded."
	firstWords := func(s string) string { return strings.Join(strings.Fields(s)[:3], " ") }
	// The recogniser hears the agent's own first words each time, as in ed25f07d ("Hola Samuel").
	stream := echoLoopStream(t, log, greeting, 850, firstWords(greeting))
	stt := stream.orch.stt.(*lengthSTT)
	said := []string{greeting + " " + bromReply2}
	for round := 0; round < 4; round++ {
		ev := waitForEventType(t, stream, BotResponse, 3*time.Second)
		said = append(said, ev.Data.(string))
		echoRoundSaying(t, stream, stt, 850, firstWords(ev.Data.(string)), log)
	}
	starts := 0
	for i, s := range said {
		if strings.HasPrefix(s, "Hello Samuel") {
			starts++
		}
		if i > 0 && len(strings.Fields(s)) >= len(strings.Fields(said[i-1])) {
			t.Fatalf("replay %d is not shorter than the one before: %q\n%s", i, said, log.dump())
		}
	}
	if starts != 1 {
		t.Fatalf("the greeting started %d times, want once: %q", starts, said)
	}
	for i, s := range said {
		t.Logf("said %d: %s", i, s)
	}
	if !log.has("INFO Saying the lost reply again: resume from word", "") {
		t.Fatalf("the resume point was not logged:\n%s", log.dump())
	}
}

// The same echo (the agent's own words, the same transcript) cutting the replay before a word of it is out, twice in
// a row: the next replay is protected, and that echo over it plays through.
func TestEchoLoop_SameEchoTwiceProtectsTheReplay(t *testing.T) {
	log := &recordingLogger{}
	stream, _, _ := loopToProtected(t, log)
	start := playoutStart(t, stream, log)
	stream.mu.Lock()
	stream.vadSpeaking = true
	stream.state = StateSpeaking
	state := stream.state
	stream.mu.Unlock()
	time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
	stream.onVADStart(state)
	stream.mu.Lock()
	// The synthetic echo matches perfectly; ed25f07d's confirmed echoes read 0.858 and 0.837 (under the 0.94 bar).
	stream.echoLoop.scores = []float64{0.858, 0.837}
	floor := stream.echoLoop.floor()
	stream.mu.Unlock()
	stream.echoMu.Lock()
	stream.echoLagKnown = false // judged against the searched-delay bar, as ed25f07d's stops were
	stream.echoMu.Unlock()
	// What the per-frame check reads must be under its own bar (as in ed25f07d: 0.84-0.91 against 0.94) and at or
	// above the loop's floor.
	var r acousticEchoReading
	found := false
	for mix := 0.0; mix <= 1.0; mix += 0.005 {
		far, near := echoBuffers(mix)
		setEchoBuffers(stream, far, near)
		r = stream.acousticEchoCheck(stream.payloadGen)
		if !r.echo && r.score >= floor {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no mix reads between the loop's floor %.3f and the echo bar", floor)
	}
	stream.stopPlayThroughIfDue(time.Now().Add(500 * time.Millisecond))
	stream.mu.Lock()
	stopped := stream.onset.stopped
	stream.mu.Unlock()
	if stopped || !log.has("INFO echo loop: replay protected, playing through the same echo", "") {
		t.Fatalf("the protected replay was stopped by its confirmed echo (score %.3f, floor %.3f):\n%s", r.score, floor, log.dump())
	}
}

// A caller who really talks over a protected replay stops it exactly as on any other reply.
func TestEchoLoop_RealSpeechStillStopsAProtectedReplay(t *testing.T) {
	log := &recordingLogger{}
	stream, _, _ := loopToProtected(t, log)
	stream.mu.Lock()
	stream.echoLoop.scores = []float64{0.858, 0.837} // ed25f07d's confirmed echoes, as in the test above
	stream.mu.Unlock()
	stream.echoMu.Lock()
	stream.echoLagKnown = false
	stream.echoMu.Unlock()
	start := playoutStart(t, stream, log)
	stream.mu.Lock()
	stream.vadSpeaking = true
	// The replay is still playing on the caller's side (the test's synthesis finishes at once; production's
	// stream is speaking while it plays).
	stream.state = StateSpeaking
	state := stream.state
	stream.mu.Unlock()
	time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
	stream.onVADStart(state)
	far, near := echoBuffers(1) // another voice, nothing of the agent's
	setEchoBuffers(stream, far, near)
	if rr := stream.acousticEchoCheck(stream.payloadGen); rr.echo || rr.score >= 0.817 {
		t.Fatalf("the other voice reads %.3f: not another voice", rr.score)
	}
	stream.stopPlayThroughIfDue(time.Now().Add(500 * time.Millisecond))
	stream.mu.Lock()
	stopped := stream.onset.stopped
	stream.mu.Unlock()
	if !stopped || !log.has("INFO Caller kept talking over the agent: stopping playback", "") {
		t.Fatalf("real speech over a protected replay did not stop it:\n%s", log.dump())
	}
	if log.has("INFO echo loop: replay protected, playing through the same echo", "") {
		t.Fatalf("real speech was taken for the echo")
	}
}

// Only the same echo, cutting the replay of the same reply, counts towards protection.
func TestEchoLoop_CountsOnlyTheSameEchoOnTheReplay(t *testing.T) {
	ms := &ManagedStream{}
	ms.noteEchoLoop(1, true, "Hola Samuel", 0.86)
	ms.echoLoop.resayGen = 2
	ms.noteEchoLoop(2, true, "hola  samuel", 0.84)
	if ms.echoLoop.count != 2 {
		t.Fatalf("count = %d, want 2", ms.echoLoop.count)
	}
	if f := ms.echoLoop.floor(); math.Abs(f-0.82) > 1e-9 {
		t.Fatalf("floor = %v, want the weakest confirmed score less the margin, 0.82", f)
	}
	ms.echoLoop.resayGen = 3
	ms.noteEchoLoop(3, true, "Sí, dime", 0.9) // other words
	if ms.echoLoop.count != 1 {
		t.Fatalf("other words kept the count at %d", ms.echoLoop.count)
	}
	ms.noteEchoLoop(7, true, "Sí, dime", 0.9) // not the replay it would cut
	if ms.echoLoop.count != 1 {
		t.Fatalf("an echo on another reply counted: %d", ms.echoLoop.count)
	}
	ms.noteEchoLoop(7, false, "Sí, dime", 0.9) // acoustic only
	if ms.echoLoop.count != 0 {
		t.Fatalf("an acoustic-only verdict kept the count at %d", ms.echoLoop.count)
	}
}
