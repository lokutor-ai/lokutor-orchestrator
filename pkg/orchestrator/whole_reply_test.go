package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func withWholeReply(t *testing.T, maxChars int, wait time.Duration) {
	t.Helper()
	on, mc, w := wholeReply, wholeReplyMaxChars, wholeReplyWait
	wholeReply, wholeReplyMaxChars, wholeReplyWait = true, maxChars, wait
	t.Cleanup(func() { wholeReply, wholeReplyMaxChars, wholeReplyWait = on, mc, w })
}

func TestGatherReplyUntilTheReplyEnds(t *testing.T) {
	q := make(chan string, 4)
	q <- "Segunda frase."
	q <- "Tercera."
	close(q)
	text, carry, n, why := gatherReply(context.Background(), "Vale.", q, 0, 0)
	if text != "Vale. Segunda frase. Tercera." || carry != "" || n != 3 || why != "end" {
		t.Fatalf("got %q carry %q n %d why %q", text, carry, n, why)
	}
}

func TestGatherReplyStopsAtAMark(t *testing.T) {
	q := make(chan string, 4)
	q <- "Lo miro."
	q <- replyFlushMark
	q <- "Después de la herramienta."
	text, _, n, why := gatherReply(context.Background(), "Un momento.", q, 0, 0)
	if text != "Un momento. Lo miro." || n != 2 || why != "mark" {
		t.Fatalf("got %q n %d why %q", text, n, why)
	}
	if rest := <-q; rest != "Después de la herramienta." {
		t.Fatalf("the sentence after the mark was consumed: %q", rest)
	}
}

func TestGatherReplyCarriesTheSentencePastTheLimit(t *testing.T) {
	q := make(chan string, 4)
	q <- "Esta frase cabe."
	q <- "Esta otra ya no cabe en el mismo trozo."
	text, carry, n, why := gatherReply(context.Background(), "Primera frase del trozo.", q, 45, 0)
	if text != "Primera frase del trozo. Esta frase cabe." || carry != "Esta otra ya no cabe en el mismo trozo." || n != 2 || why != "max_chars" {
		t.Fatalf("got %q carry %q n %d why %q", text, carry, n, why)
	}
}

func TestGatherReplyWaitIsBounded(t *testing.T) {
	q := make(chan string)
	start := time.Now()
	text, _, _, why := gatherReply(context.Background(), "Sola.", q, 0, 40*time.Millisecond)
	if text != "Sola." || why != "wait" || time.Since(start) > time.Second {
		t.Fatalf("got %q why %q after %v", text, why, time.Since(start))
	}
}

func TestGatherReplyEndsWhenTheTurnIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	text, _, _, why := gatherReply(ctx, "Sola.", make(chan string), 0, 0)
	if text != "Sola." || why != "cancelled" {
		t.Fatalf("got %q why %q", text, why)
	}
}

func TestGroupWholeReply(t *testing.T) {
	segs := []string{"Vale.", "Te lo miro ahora.", "Tengo hueco el martes a las diez.", "¿Te va bien?"}
	if got := groupWholeReply(segs, 0); !reflect.DeepEqual(got, []string{strings.Join(segs, " ")}) {
		t.Fatalf("no limit: %q", got)
	}
	want := []string{"Vale. Te lo miro ahora.", "Tengo hueco el martes a las diez.", "¿Te va bien?"}
	if got := groupWholeReply(segs, 40); !reflect.DeepEqual(got, want) {
		t.Fatalf("limit 40: %q", got)
	}
}

// With the flag on, a streamed reply is one synthesis, whatever its sentences; off, one per sentence as before.
func TestWholeReply_OneSynthesisForTheReply(t *testing.T) {
	chunks := []string{s1 + " ", s2 + " ", s3}
	off := newArriverTTS(false)
	runReply(t, off, chunks)
	if got := off.snapshot(); len(got) != 3 {
		t.Fatalf("flag off: want one synthesis per sentence, got %q", got)
	}

	withWholeReply(t, 0, 0)
	on := newArriverTTS(false)
	runReply(t, on, chunks)
	got := on.snapshot()
	if len(got) != 1 || got[0][0] != s1+" "+s2+" "+s3 {
		t.Fatalf("flag on: want the reply as one synthesis, got %q", got)
	}
}

// A long reply goes in as few pieces as the limit allows, cut between sentences, in order, nothing lost.
func TestWholeReply_LongReplyInChunks(t *testing.T) {
	withWholeReply(t, len(s1)+len(s2)+5, 0)
	tts := newArriverTTS(false)
	runReply(t, tts, []string{s1 + " ", s2 + " ", s3})
	got := tts.snapshot()
	if len(got) != 2 || got[0][0] != s1+" "+s2 || got[1][0] != s3 {
		t.Fatalf("got %q", got)
	}
}

// Whole-reply takes the place of arrivals when both are on: the reply is one text, nothing is appended to it.
func TestWholeReply_BeforeArrivals(t *testing.T) {
	withWholeReply(t, 0, 0)
	tts := newArriverTTS(true)
	runReply(t, tts, []string{s1 + " ", s2})
	got := tts.snapshot()
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != s1+" "+s2 {
		t.Fatalf("got %q", got)
	}
}
