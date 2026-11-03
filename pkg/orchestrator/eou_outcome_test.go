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
	ms.logEOUDecision(3, TranscriptionResult{}, "hello there friend", "waited", 700, true)
	ms.resolveEOUOutcome(time.Now().Add(900 * time.Millisecond))
	o := lg.outcomes()
	if len(o) != 1 || o[0]["resumed"] != true || o[0]["turn_seq"] != 3 {
		t.Fatalf("outcomes = %v", o)
	}
}

func TestEOUOutcomeNotResumedWhenTheNextUtteranceIsLater(t *testing.T) {
	ms, lg := newEOUStream()
	ms.logEOUDecision(1, TranscriptionResult{}, "hi", "answer_now", 0, true)
	ms.resolveEOUOutcome(time.Now().Add(5 * time.Second))
	if o := lg.outcomes(); len(o) != 1 || o[0]["resumed"] != false {
		t.Fatalf("outcomes = %v", o)
	}
}

func TestEOUOutcomeClosesEveryDecisionOnce(t *testing.T) {
	ms, lg := newEOUStream()
	ms.logEOUDecision(1, TranscriptionResult{}, "a", "answer_now", 0, true)
	ms.logEOUDecision(2, TranscriptionResult{}, "b", "answer_now", 0, true) // 1 closed: nothing followed it
	ms.flushEOUOutcome()                                                    // 2 closed at the end of the session
	ms.flushEOUOutcome()                                                    // no double line
	o := lg.outcomes()
	if len(o) != 2 || o[0]["turn_seq"] != 1 || o[1]["turn_seq"] != 2 || o[0]["resumed"] != false || o[1]["resumed"] != false {
		t.Fatalf("outcomes = %v", o)
	}
}
