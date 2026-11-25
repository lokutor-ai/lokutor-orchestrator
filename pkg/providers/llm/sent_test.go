package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func textChunk(s string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", s)
}

const usageChunk = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20,\"total_tokens\":1020}}\n\ndata: [DONE]\n\n"

// talker is an OpenRouter provider on a test server.
func talker(url, model string) *OpenRouterLLM {
	l := NewOpenRouterLLM("k", model, nil, "none")
	l.url = url
	return l
}

func meterCtx() (context.Context, *orchestrator.SessionMeter) {
	m := orchestrator.NewSessionMeter()
	return orchestrator.WithSessionMeter(context.Background(), m), m
}

func say(n int) []orchestrator.Message {
	return []orchestrator.Message{{Role: "system", Content: strings.Repeat("word ", 400)}, {Role: "user", Content: strings.Repeat("x", n)}}
}

// streamThenHang answers with text and then says nothing more until the client goes.
func streamThenHang(text string, reached chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, textChunk(text))
		w.(http.Flusher).Flush()
		if reached != nil {
			reached <- struct{}{}
		}
		<-r.Context().Done()
	}
}

// A reply cancelled before its usage arrives is counted at the session's own tokens per character, and what arrived of it.
func TestCancelledReplyIsCountedAsAnEstimate(t *testing.T) {
	calls := 0
	var hang http.HandlerFunc
	reached := make(chan struct{}, 1)
	hang = streamThenHang(strings.Repeat("a", 100), reached)
	var firstChars int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			b, _ := io.ReadAll(r.Body)
			firstChars = len([]rune(string(b)))
			fmt.Fprint(w, textChunk("hello")+usageChunk)
			return
		}
		hang(w, r)
	}))
	defer srv.Close()
	ctx, m := meterCtx()
	l := talker(srv.URL, "openai/gpt-x")
	if _, err := l.StreamComplete(ctx, say(10), nil, func(string) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot()["reply"]; got.Prompt != 1000 || got.EstPrompt != 0 {
		t.Fatalf("reported: %+v", got)
	}

	cctx, cancel := context.WithCancel(orchestrator.WithTokenCategory(ctx, orchestrator.TokensSpeculative))
	go func() { <-reached; time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := l.StreamComplete(cctx, say(1010), nil, func(string) error { return nil }, nil); err == nil {
		t.Fatal("a cancelled stream returned no error")
	}
	got := m.Snapshot()["speculative"]
	want := int64(float64(1000) / float64(firstChars) * float64(firstChars+1000))
	if d := got.Prompt - want; d < -3 || d > 3 {
		t.Fatalf("prompt = %d, want about %d (%+v)", got.Prompt, want, got)
	}
	if got.Completion != 25 || got.EstPrompt != got.Prompt || got.EstCompletion != 25 || got.Requests != 1 {
		t.Fatalf("speculative = %+v", got)
	}
}

// Cancelled while the model was still reading: the prompt is counted, at the model's usual ratio. Never sent, nothing.
func TestCancelledBeforeTheAnswerBegan(t *testing.T) {
	in := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		in <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	for _, l := range []struct {
		name string
		p    orchestrator.StreamingLLMProvider
		rate float64
	}{
		{"claude", talker(srv.URL, "anthropic/claude-haiku-5.5"), 0.385},
		{"other", talker(srv.URL, "deepseek/deepseek-v4.1-flash"), 0.25},
		{"cerebras", func() *CerebrasLLM { c := NewCerebrasLLM("k", ""); c.url = srv.URL; return c }(), 0.25},
		{"groq", func() *GroqLLM { c := NewGroqLLM("k", ""); c.url = srv.URL; return c }(), 0.25},
	} {
		ctx, m := meterCtx()
		cctx, cancel := context.WithCancel(ctx)
		go func() { <-in; cancel() }()
		if _, err := l.p.StreamComplete(cctx, say(0), nil, nil, nil); err == nil {
			t.Fatal("no error")
		}
		got := m.Snapshot()["reply"]
		if low, high := int64(2000*l.rate), int64(2600*l.rate); got.Prompt < low || got.Prompt > high || got.Completion != 0 || got.EstPrompt != got.Prompt {
			t.Fatalf("%s: %+v, want %d to %d", l.name, got, low, high)
		}

		gone, stop := context.WithCancel(ctx)
		stop()
		l.p.StreamComplete(gone, say(0), nil, nil, nil)
		if again := m.Snapshot()["reply"]; again != got {
			t.Fatalf("%s: a request that never left was counted: %+v", l.name, again)
		}
	}
}

// What the provider refuses, breaks off or is too slow to take is its own failure and counts nothing.
func TestAProvidersFailureIsNotCounted(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"refused": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 429) },
		"error chunk": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, textChunk("a half")+"data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
		},
		"broken off": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100000")
			fmt.Fprint(w, textChunk("a half"))
		},
		"never answered": func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() },
	} {
		srv := httptest.NewServer(h)
		ctx, m := meterCtx()
		tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		if _, err := talker(srv.URL, "openai/gpt-x").StreamComplete(tctx, say(0), nil, func(string) error { return nil }, nil); err == nil {
			t.Fatalf("%s: no error", name)
		}
		cancel()
		if got := m.Snapshot(); len(got) != 0 {
			t.Fatalf("%s: counted %+v", name, got)
		}
		srv.Close()
	}
}

// A reader that has had enough stops the stream itself, and a host that sends no usage ends it without one.
func TestStoppedAndUnreported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textChunk(strings.Repeat("b", 40))+"data: [DONE]\n\n")
	}))
	defer srv.Close()
	ctx, m := meterCtx()
	l := talker(srv.URL, "openai/gpt-x")
	l.StreamComplete(orchestrator.WithTokenCategory(ctx, "a"), say(0), nil, func(string) error { return errors.New("superseded") }, nil)
	l.StreamComplete(orchestrator.WithTokenCategory(ctx, "b"), say(0), nil, func(string) error { return nil }, nil)
	for _, c := range []string{"a", "b"} {
		if got := m.Snapshot()[c]; got.EstPrompt < 500 || got.EstCompletion != 10 {
			t.Fatalf("%s: %+v", c, got)
		}
	}
}

// Of two attempts raced, the one that lost is the platform's; a turn cancelled before either answered is counted once.
func TestHedgeIsCountedOnce(t *testing.T) {
	t.Setenv("LLM_HEDGE_MS", "30")
	reached := make(chan struct{}, 4)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		reached <- struct{}{}
		<-r.Context().Done()
	}))
	defer slow.Close()
	quick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textChunk("yes")+usageChunk)
	}))
	defer quick.Close()

	ctx, m := meterCtx()
	c := NewChainLLM("t", talker(slow.URL, "a/slow"), talker(quick.URL, "b/quick"))
	if text, err := c.StreamComplete(ctx, say(0), nil, func(string) error { return nil }, nil); err != nil || text != "yes" {
		t.Fatal(text, err)
	}
	m.Settle(2 * time.Second)
	if got := m.Snapshot()["reply"]; got.Requests != 1 || got.EstPrompt != 0 || got.Prompt != 1000 {
		t.Fatalf("the loser was counted: %+v", got)
	}

	ctx, m = meterCtx()
	cctx, cancel := context.WithCancel(ctx)
	go func() { <-reached; <-reached; cancel() }()
	c = NewChainLLM("t", talker(slow.URL, "a/slow"), talker(slow.URL, "b/slow"))
	<-reached // the first run's
	c.StreamComplete(cctx, say(0), nil, func(string) error { return nil }, nil)
	m.Settle(2 * time.Second)
	if got := m.Snapshot()["reply"]; got.Requests != 1 || got.EstPrompt == 0 {
		t.Fatalf("a cancelled race: %+v", got)
	}
}

// A bill drawn as the call ends waits for the request the hang-up cancelled.
func TestSettleWaitsForOpenRequests(t *testing.T) {
	m := orchestrator.NewSessionMeter()
	m.Settle(time.Minute) // nothing open: returns at once
	done := m.Begin()
	go func() { time.Sleep(30 * time.Millisecond); m.AddEstimate("reply", 10, 0); done(); done() }()
	start := time.Now()
	m.Settle(5 * time.Second)
	if m.Snapshot()["reply"].Prompt != 10 || time.Since(start) > 3*time.Second {
		t.Fatal("did not wait for the open request")
	}
	m.Begin()
	start = time.Now()
	m.Settle(50 * time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("waited past its limit")
	}
}

// A prompt sent once is not written to the cache: the write costs more than the input and nothing reads it back.
func TestAPromptSentOnceIsNotCached(t *testing.T) {
	l := NewOpenRouterLLM("k", "anthropic/claude-sonnet-5.5", nil, "none")
	if _, ok := l.request(context.Background(), say(1), nil, false)["cache_control"]; !ok {
		t.Fatal("no cache mark by default")
	}
	p := l.request(orchestrator.WithSentOnce(context.Background()), say(1), nil, false)
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "cache_control") {
		t.Fatal("marked: ", string(b))
	}
}
