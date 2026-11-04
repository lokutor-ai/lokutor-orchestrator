package orchestrator

import (
	"sync"
	"testing"
	"time"
)

type capLogger struct {
	mu    sync.Mutex
	lines []map[string]interface{}
}

func (l *capLogger) add(msg string, args []interface{}) {
	m := map[string]interface{}{"msg": msg}
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			m[k] = args[i+1]
		}
	}
	l.mu.Lock()
	l.lines = append(l.lines, m)
	l.mu.Unlock()
}
func (l *capLogger) Debug(string, ...interface{})       {}
func (l *capLogger) Info(msg string, a ...interface{})  { l.add(msg, a) }
func (l *capLogger) Warn(msg string, a ...interface{})  {}
func (l *capLogger) Error(msg string, a ...interface{}) {}
func (l *capLogger) outcomes() (out []map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.lines {
		if m["msg"] == "eou_outcome" {
			out = append(out, m)
		}
	}
	return
}

func newEOUStream() (*ManagedStream, *capLogger) {
	lg := &capLogger{}
	return &ManagedStream{logger: lg, orch: &Orchestrator{config: Config{}}}, lg
}

func TestEOUOutcomeResumedWithinTheWindow(t *testing.T) {
	ms, lg := newEOUStream()
	ms.logEOUDecision(3, TranscriptionResult{}, "hello there friend", "waited", 700, true, [4]float32{}, 0, 0)
	ms.resolveEOUOutcome(time.Now().Add(900 * time.Millisecond))
	o := lg.outcomes()
	if len(o) != 1 || o[0]["resumed"] != true || o[0]["turn_seq"] != 3 {
		t.Fatalf("outcomes = %v", o)
	}
}

func TestEOUOutcomeNotResumedWhenTheNextUtteranceIsLater(t *testing.T) {
	ms, lg := newEOUStream()
	ms.logEOUDecision(1, TranscriptionResult{}, "hi", "answer_now", 0, true, [4]float32{}, 0, 0)
	ms.resolveEOUOutcome(time.Now().Add(5 * time.Second))
	if o := lg.outcomes(); len(o) != 1 || o[0]["resumed"] != false {
		t.Fatalf("outcomes = %v", o)
	}
}

func TestEOUOutcomeClosesEveryDecisionOnce(t *testing.T) {
	ms, lg := newEOUStream()
	ms.logEOUDecision(1, TranscriptionResult{}, "a", "answer_now", 0, true, [4]float32{}, 0, 0)
	ms.logEOUDecision(2, TranscriptionResult{}, "b", "answer_now", 0, true, [4]float32{}, 0, 0) // 1 closed: nothing followed it
	ms.flushEOUOutcome()                                                    // 2 closed at the end of the session
	ms.flushEOUOutcome()                                                    // no double line
	o := lg.outcomes()
	if len(o) != 2 || o[0]["turn_seq"] != 1 || o[1]["turn_seq"] != 2 || o[0]["resumed"] != false || o[1]["resumed"] != false {
		t.Fatalf("outcomes = %v", o)
	}
}

func TestAdaptivePatienceGrowsPerResumeAndIsCapped(t *testing.T) {
	ms, _ := newEOUStream()
	ms.orch.config.EndOfTurnAdaptStepMs, ms.orch.config.EndOfTurnAdaptMaxMs = 400, 1000
	if ms.eouAdaptMs() != 0 {
		t.Fatal("a caller who has not resumed earns nothing")
	}
	for i := 1; i <= 3; i++ {
		ms.logEOUDecision(i, TranscriptionResult{}, "a b c", "waited", 700, true, [4]float32{}, 0, 0)
		ms.resolveEOUOutcome(time.Now().Add(300 * time.Millisecond)) // resumed
	}
	if got := ms.eouAdaptMs(); got != 1000 {
		t.Fatalf("3 resumes of 400 ms capped at 1000, got %d", got)
	}
	ms.orch.config.EndOfTurnAdaptStepMs = 0
	if ms.eouAdaptMs() != 0 {
		t.Fatal("step 0 is off")
	}
}

func TestAdaptivePatienceDecaysAfterThreeCalmDecisions(t *testing.T) {
	ms, _ := newEOUStream()
	ms.orch.config.EndOfTurnAdaptStepMs, ms.orch.config.EndOfTurnAdaptMaxMs = 400, 1600
	ms.logEOUDecision(1, TranscriptionResult{}, "a b c", "waited", 700, true, [4]float32{}, 0, 0)
	ms.resolveEOUOutcome(time.Now().Add(200 * time.Millisecond)) // one resume: 400 ms earned
	if ms.eouAdaptMs() != 400 {
		t.Fatalf("want 400, got %d", ms.eouAdaptMs())
	}
	for i := 2; i <= 4; i++ {
		ms.logEOUDecision(i, TranscriptionResult{}, "a b c", "answer_now", 0, true, [4]float32{}, 0, 0)
		ms.resolveEOUOutcome(time.Now().Add(9 * time.Second)) // nobody resumed
	}
	if ms.eouAdaptMs() != 0 {
		t.Fatalf("three calm decisions in a row should take the event back, got %d", ms.eouAdaptMs())
	}
}

func TestOpenScoreTakesTheLowestHead(t *testing.T) {
	r := TranscriptionResult{HasEndOfTurnText: true, EndOfTurnText: 0.6, HasEndOfTurnAlt: true, EndOfTurnAlt: 0.005}
	if got, _ := openScore(r, 0.84); got != 0.6 {
		t.Errorf("the text head is the one that counts, not the second audio head, got %v", got)
	}
	if got, _ := openScore(TranscriptionResult{HasEndOfTurnAlt: true, EndOfTurnAlt: 0.01}, 0.84); got != 0.01 {
		t.Errorf("with no text head the second audio head stands in, got %v", got)
	}
	if got, _ := openScore(TranscriptionResult{}, 0.84); got != 0.84 {
		t.Errorf("no other head: the deciding probability, got %v", got)
	}
}
