package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestShortOpenerMergedWithNext(t *testing.T) {
	q := make(chan string, 2)
	q <- "¿Y cuántos años estuviste en ese almacén?"
	got, merged := joinShortOpener(context.Background(), "Vale.", q, 500*time.Millisecond)
	if !merged || got != "Vale. ¿Y cuántos años estuviste en ese almacén?" {
		t.Fatalf("got %q merged=%v", got, merged)
	}
}

func TestShortOpenersCovered(t *testing.T) {
	for _, s := range []string{"Vale.", "Claro.", "Perfecto.", "Vale, seguimos.", "Sí.", "Okay.", "Perfecto, gracias."} {
		if !isShortOpener(s) {
			t.Errorf("%q should count as a short opener", s)
		}
	}
	for _, s := range []string{"Vale, empezamos con unas preguntas clave.", "Perfecto, te lo apunto ahora."} {
		if isShortOpener(s) {
			t.Errorf("%q should be spoken on its own", s)
		}
	}
}

func TestShortOpenerAloneAtReplyEnd(t *testing.T) {
	q := make(chan string)
	close(q)
	got, merged := joinShortOpener(context.Background(), "Vale.", q, time.Second)
	if merged || got != "Vale." {
		t.Fatalf("got %q merged=%v", got, merged)
	}
}

func TestShortOpenerWaitIsCapped(t *testing.T) {
	q := make(chan string) // nothing ever arrives
	start := time.Now()
	got, merged := joinShortOpener(context.Background(), "Vale.", q, 80*time.Millisecond)
	if merged || got != "Vale." {
		t.Fatalf("got %q merged=%v", got, merged)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("waited %v, cap was 80ms", d)
	}
}

func TestNormalFirstSentenceUnchanged(t *testing.T) {
	q := make(chan string, 1)
	q <- "Second sentence."
	first := "Vale, empezamos con unas preguntas clave que necesito confirmar contigo."
	got, merged := joinShortOpener(context.Background(), first, q, time.Second)
	if merged || got != first {
		t.Fatalf("got %q merged=%v", got, merged)
	}
	if len(q) != 1 {
		t.Fatal("the next sentence must stay queued")
	}
}

func TestShortOpenerBargeInWhileWaiting(t *testing.T) {
	q := make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	got, merged := joinShortOpener(ctx, "Vale.", q, 5*time.Second)
	if merged || got != "Vale." || time.Since(start) > time.Second {
		t.Fatalf("got %q merged=%v after %v", got, merged, time.Since(start))
	}
}

func TestShortOpenerSegments(t *testing.T) {
	got := joinShortOpenerSegments([]string{"Claro.", "Te lo miro ahora.", "Un momento."})
	if len(got) != 2 || got[0] != "Claro. Te lo miro ahora." || got[1] != "Un momento." {
		t.Fatalf("got %q", got)
	}
	long := []string{"Perfecto, te lo apunto para el jueves.", "Gracias."}
	if g := joinShortOpenerSegments(long); len(g) != 2 {
		t.Fatalf("a normal first sentence must stay alone: %q", g)
	}
}
