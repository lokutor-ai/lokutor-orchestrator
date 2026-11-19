package orchestrator

import "strings"

// An echo loop (2026-11-19, call ed25f07d on 2026-11-17): the agent's greeting came back through the caller's
// microphone, a pending barge-in stopped playback at 400 ms before the acoustic check could match it (0.84-0.91
// against a bar of 0.94), the transcript then proved it was the agent's own words ("Hola Samuel"), and the lost
// reply was said again: from the start of the cut sentence, which was the greeting, which echoed again. Six times,
// and the call never got past it.
//
// Two things end it. The replay starts at the word that was cut (lostReplyLocked), so each round gets further.
// And once the SAME echo (the agent's own words, the same transcript) has cut the replay of that reply twice in a
// row, the next replay is protected: speech over it that sounds as much like the agent's audio as that confirmed
// echo did plays through instead of stopping it (stopPlayThroughIfDue). A caller saying anything else breaks the
// match and stops playback as on any other reply; the protection ends with that replay.

// echoLoopMargin is how far below the weakest confirmed echo's acoustic score speech over a protected replay may
// read and still be taken for that echo. Never below echoTextAcousticFloor.
const echoLoopMargin = 0.02

type echoLoopState struct {
	// text is the confirmed echo's words (normalised), count how many times in a row it cut the replay of the
	// reply it was cutting, scores its acoustic scores, resayGen the replay it would cut next, protectGen the
	// replay that is protected (0: none).
	text       string
	count      int
	scores     []float64
	resayGen   int
	protectGen int
}

// floor is the acoustic score speech over a protected replay must reach to be taken for the confirmed echo.
func (e *echoLoopState) floor() float64 {
	if len(e.scores) == 0 {
		return 1.1
	}
	min := e.scores[0]
	for _, s := range e.scores[1:] {
		if s < min {
			min = s
		}
	}
	f := min - echoLoopMargin
	if f < echoTextAcousticFloor {
		f = echoTextAcousticFloor
	}
	return f
}

// noteEchoLoop records an echo verdict that is about to have the lost reply said again (settleEchoVeto): it is
// the same loop when the echo was the agent's own words, with the same transcript as last time, and it cut the
// previous replay of this reply. Anything else starts the count over.
func (ms *ManagedStream) noteEchoLoop(gen int, textEcho bool, transcript string, score float64) {
	key := strings.ToLower(strings.Join(strings.Fields(transcript), " "))
	ms.mu.Lock()
	defer ms.mu.Unlock()
	e := &ms.echoLoop
	if !textEcho || key == "" {
		*e = echoLoopState{}
		return
	}
	if e.count > 0 && e.text == key && e.resayGen == gen {
		e.count++
		e.scores = append(e.scores, score)
		return
	}
	*e = echoLoopState{text: key, count: 1, scores: []float64{score}}
}
