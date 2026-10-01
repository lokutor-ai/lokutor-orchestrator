package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) add(level, msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(args...))
}
func (l *recordingLogger) Debug(msg string, args ...interface{}) { l.add("DEBUG", msg, args...) }
func (l *recordingLogger) Info(msg string, args ...interface{})  { l.add("INFO", msg, args...) }
func (l *recordingLogger) Warn(msg string, args ...interface{})  { l.add("WARN", msg, args...) }
func (l *recordingLogger) Error(msg string, args ...interface{}) { l.add("ERROR", msg, args...) }

func (l *recordingLogger) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestToolResultError(t *testing.T) {
	cases := map[string]string{
		`{"error": "the calendar could not be reached"}`:              "the calendar could not be reached",
		`{"error": {"code": 500}}`:                                    `{"code": 500}`,
		`{"booked": true, "error": null}`:                             "",
		`{"booked": false, "reason": "that time has already passed"}`: "",
		`{"error": ""}`:     "",
		`plain text answer`: "",
		`[1,2]`:             "",
		`{not json`:         "",
	}
	for in, want := range cases {
		if got := toolResultError(in); got != want {
			t.Errorf("toolResultError(%q) = %q, want %q", in, got, want)
		}
	}
}

// A tool that fails is in the log, whichever way it fails: an {"error": ...} result (how every handler
// here reports one) or a Go error.
func TestToolCallOutcomeIsLogged(t *testing.T) {
	log := &recordingLogger{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "hi"}, &sequencedLLM{}, &blockingAfterFirstTTS{}, nil, DefaultConfig(), log)
	orch.RegisterTool("check_availability", func(args string) (string, error) {
		return `{"error": "the calendar could not be reached"}`, nil
	})
	orch.RegisterTool("lookup", func(args string) (string, error) { return "", errors.New("crm down") })
	orch.RegisterTool("book_appointment", func(args string) (string, error) { return `{"booked": true}`, nil })
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("tools"))
	t.Cleanup(ms.Close)

	for _, name := range []string{"check_availability", "lookup", "book_appointment"} {
		ms.dispatchToolCall(context.Background(), ToolCallEventData{Name: name, CallID: name})
	}
	if n := log.count("WARN Tool call failed"); n != 2 {
		t.Fatalf("expected the two failures logged, got %d: %v", n, log.lines)
	}
	if n := log.count("INFO Tool call "); n != 1 {
		t.Fatalf("expected the success logged once, got %d: %v", n, log.lines)
	}
}
