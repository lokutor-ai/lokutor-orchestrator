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
	on, mc, w, fw := wholeReply, wholeReplyMaxChars, wholeReplyWait, wholeReplyFirstWait
	wholeReply, wholeReplyMaxChars, wholeReplyWait, wholeReplyFirstWait = true, maxChars, wait, wait
	t.Cleanup(func() { wholeReply, wholeReplyMaxChars, wholeReplyWait, wholeReplyFirstWait = on, mc, w, fw })
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

// slowLLM streams a reply's chunks with a pause before each.
type slowLLM struct {
	chunks []string
	gap    time.Duration
}

func (l *slowLLM) Complete(ctx context.Context, m []Message, t []Tool) (string, error) {
	return strings.Join(l.chunks, ""), nil
}
func (l *slowLLM) Name() string { return "slowLLM" }
func (l *slowLLM) StreamComplete(ctx context.Context, m []Message, t []Tool, onChunk func(string) error, onTool func(ToolCallEventData) error) (string, error) {
	for i, c := range l.chunks {
		if i > 0 {
			time.Sleep(l.gap)
		}
		if err := onChunk(c); err != nil {
			return "", err
		}
	}
	return strings.Join(l.chunks, ""), nil
}

func runSlowReply(t *testing.T, tts TTSProvider, llm *slowLLM) {
	t.Helper()
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("whole"))
	done := make(chan struct{})
	go func() { ms.runLLMAndTTS(context.Background(), "hola"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reply did not finish")
	}
}

// With a short first wait the reply is two pieces: what was written when the first sentence was ready, then
// everything written while that was being synthesised. First audio does not wait for the reply.
func TestWholeReply_TwoPiecesWithoutWaitingForTheReply(t *testing.T) {
	withWholeReply(t, 0, 0)
	wholeReplyFirstWait = 5 * time.Millisecond
	tts := newArriverTTS(false)
	tts.perSentence, tts.delay = 10, 20*time.Millisecond // the first piece takes 200 ms to synthesise
	runSlowReply(t, tts, &slowLLM{chunks: []string{s1 + " ", s2 + " ", s3 + " ", s4}, gap: 40 * time.Millisecond})
	got := tts.snapshot()
	if len(got) != 2 || got[0][0] != s1 || got[1][0] != s2+" "+s3+" "+s4 {
		t.Fatalf("got %q", got)
	}
}

// A short opener still waits for its sentence when the first wait runs out.
func TestWholeReply_ShortOpenerWaitsForItsSentence(t *testing.T) {
	withWholeReply(t, 0, 0)
	wholeReplyFirstWait = 5 * time.Millisecond
	tts := newArriverTTS(false)
	tts.perSentence, tts.delay = 10, 20*time.Millisecond
	runSlowReply(t, tts, &slowLLM{chunks: []string{"Vale. ", s2 + " ", s3 + " ", s4}, gap: 40 * time.Millisecond})
	got := tts.snapshot()
	if len(got) != 2 || got[0][0] != "Vale. "+s2 || got[1][0] != s3+" "+s4 {
		t.Fatalf("got %q", got)
	}
}
