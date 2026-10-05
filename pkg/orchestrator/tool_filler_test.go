package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestToolFillerPools(t *testing.T) {
	for _, lang := range []Language{LanguageEn, LanguageEs, LanguageFr, LanguageDe, LanguageIt, LanguagePt} {
		pool := toolFillerPool(lang)
		if len(pool) < 6 {
			t.Errorf("%v: %d lines, want enough variety (6+)", lang, len(pool))
		}
		seen := map[string]bool{}
		for _, s := range pool {
			if seen[s] {
				t.Errorf("%v: %q appears twice", lang, s)
			}
			seen[s] = true
			if n := len(strings.Fields(s)); n < 3 {
				t.Errorf("%v: %q has %d words; the synthesiser needs 3+ to hold the language", lang, s, n)
			}
			if utf8.RuneCountInString(s) > 40 {
				t.Errorf("%v: %q is long for something said while a tool runs", lang, s)
			}
			if !strings.HasSuffix(s, ".") {
				t.Errorf("%v: %q should end the sentence", lang, s)
			}
		}
	}
	// Languages without lines of their own keep the English pool, as the single English line did.
	for _, lang := range []Language{LanguageCa, LanguageGl, LanguageEu} {
		if got, want := toolFillerPool(lang)[0], toolFillerPool(LanguageEn)[0]; got != want {
			t.Errorf("%v: %q, want the English pool", lang, got)
		}
	}
}

// Never the same line twice running, whatever the dice say, and every line is reachable.
func TestFillerRotationNeverRepeatsAndCoversThePool(t *testing.T) {
	pool := toolFillerPool(LanguageEs)
	for pin := 0; pin < len(pool); pin++ {
		f := &fillerRotation{intn: func(int) int { return pin }} // the dice always land on the same entry
		prev := ""
		for i := 0; i < 5; i++ {
			got := f.next(pool)
			if got == "" || got == prev {
				t.Fatalf("pin %d draw %d: %q after %q", pin, i, got, prev)
			}
			prev = got
		}
	}

	f := &fillerRotation{}
	seen := map[string]int{}
	prev := ""
	for i := 0; i < 2000; i++ {
		got := f.next(pool)
		if got == prev {
			t.Fatalf("draw %d repeated %q", i, got)
		}
		prev = got
		seen[got]++
	}
	for _, s := range pool {
		if seen[s] == 0 {
			t.Errorf("%q never chosen in 2000 draws", s)
		}
	}
}

func TestFillerRotationDegenerateCases(t *testing.T) {
	f := &fillerRotation{}
	if got := f.next(nil); got != "" {
		t.Errorf("empty pool gave %q", got)
	}
	// A pool of one can only repeat.
	if a, b := f.next([]string{"Un momento."}), f.next([]string{"Un momento."}); a != "Un momento." || b != a {
		t.Errorf("single-line pool gave %q, %q", a, b)
	}
}

// A caller who speaks over the agent while a client tool is out ends the turn after a few hundred
// milliseconds. That is a cancellation: it was logged as "Tool call failed ... timed out after 10
// seconds", and five of them in an hour raised the tool-calls-failing alarm on 2026-10-05.
func TestSupersededClientToolIsACancellationNotAFailure(t *testing.T) {
	log := &recordingLogger{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{}, &blockingAfterFirstTTS{}, nil, DefaultConfig(), log)
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("barge-in"))
	t.Cleanup(ms.Close)

	turnCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel() // the caller started speaking again
	}()
	start := time.Now()
	res := ms.dispatchToolCall(turnCtx, ToolCallEventData{Name: "observe", CallID: "observe-1"}) // a client tool: no handler
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("returned after %v; a superseded turn must not wait out the 10 s timeout", took)
	}
	if got := toolResultError(res); got != "cancelled" {
		t.Fatalf("result error %q, want cancelled", got)
	}
	if n := log.count("WARN Tool call failed"); n != 0 {
		t.Fatalf("a barge-in was logged as a failed tool call: %v", log.lines)
	}
	if n := log.count("INFO Tool call cancelled: the turn was superseded"); n != 1 {
		t.Fatalf("the cancellation left no evidence (CLAUDE.md rule 6): %v", log.lines)
	}
}
