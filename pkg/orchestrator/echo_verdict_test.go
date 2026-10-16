package orchestrator

import "testing"

// Punctuation separates words: an em dash glued "that—could" into one word and kept an echo under the bar
// (production, 2026-10-16: "Didn't touch that. Could you say it?" over "Sorry, I didn’t catch that—could you say it
// again?" was answered as the caller). Apostrophes, straight or curly, join a contraction either way.
func TestEchoTextKeepsWordsApartAtPunctuation(t *testing.T) {
	if got := normalizeForEchoCompare("Sorry, I didn’t catch that—could you say it again?"); got != "sorry i didnt catch that could you say it again" {
		t.Fatalf("normalized %q", got)
	}
	if !isLikelyEcho("Didn't touch that. Could you say it?", "Sorry, I didn’t catch that—could you say it again?") {
		t.Fatal("the agent's own sentence through the microphone was not recognised as its words")
	}
	if isLikelyEcho("Eight on the terrace, please.", "Which time and seating would you prefer?") {
		t.Fatal("a caller's answer was taken for the agent's words")
	}
}

// The agent's own words count as echo only with some acoustic support: a caller can repeat the agent, its echo
// sounds like the agent.
func TestEchoVerdict(t *testing.T) {
	cases := []struct {
		acoustic bool
		score    float64
		text     bool
		echo     bool
		method   string
	}{
		{true, 0.9, false, true, "acoustic"},
		{false, 0.69, true, true, "text+acoustic"},
		{false, 0.5, true, true, "text+acoustic"},
		{false, 0.29, true, false, ""},
		{false, 0.7, false, false, ""},
		{false, 0, false, false, ""},
	}
	for _, c := range cases {
		echo, method := echoVerdict(c.acoustic, c.score, c.text)
		if echo != c.echo || method != c.method {
			t.Fatalf("echoVerdict(%v, %.2f, %v) = %v %q, want %v %q", c.acoustic, c.score, c.text, echo, method, c.echo, c.method)
		}
	}
}

// The text check sees every sentence of the reply begun so far, not only the one being synthesised: the echo of the
// sentence before arrives while the next is already playing. A new reply starts the record again.
func TestEchoReplyTextIsTheWholeReplySoFar(t *testing.T) {
	ms := &ManagedStream{}
	ms.beginSegmentLocked(3, "Hey there!")
	ms.beginSegmentLocked(3, "What are you building today?")
	if ms.echoReplyGen != 3 || !isLikelyEcho("Hey there.", ms.echoReplyText) {
		t.Fatalf("reply so far %q (gen %d)", ms.echoReplyText, ms.echoReplyGen)
	}
	ms.beginSegmentLocked(4, "Sure.")
	if ms.echoReplyText != "Sure." || ms.echoReplyGen != 4 {
		t.Fatalf("a new reply kept the old one: %q", ms.echoReplyText)
	}
}
