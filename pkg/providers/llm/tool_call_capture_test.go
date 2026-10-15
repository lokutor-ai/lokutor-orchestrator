package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// Complete returns the words; with a capture on the context, the tool calls of the same reply reach it too (the
// speculative run used to lose them). Arguments sent as an object, not the format's string, must not fail the decode.
func TestCompleteHandsToolCallsToTheCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"Un momento, lo miro.","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"check_availability","arguments":"{\"day\":\"lunes\"}"}},
			{"id":"c2","type":"function","function":{"name":"lookup","arguments":{"q":"x"}}}]}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	l := NewCerebrasLLM("k", "gpt-oss-120b")
	l.url = srv.URL
	ctx, capture := orchestrator.WithToolCallCapture(context.Background())
	text, err := l.Complete(ctx, []orchestrator.Message{{Role: "user", Content: "¿tenéis hueco el lunes?"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Un momento, lo miro." {
		t.Errorf("text %q", text)
	}
	calls := capture.Calls()
	if len(calls) != 2 || calls[0].Name != "check_availability" || calls[0].Arguments != `{"day":"lunes"}` || calls[0].CallID != "c1" ||
		calls[1].Name != "lookup" || calls[1].Arguments != `{"q":"x"}` {
		t.Errorf("calls %+v", calls)
	}

	// Without a capture nothing changes.
	text, err = l.Complete(context.Background(), []orchestrator.Message{{Role: "user", Content: "x"}}, nil)
	if err != nil || text != "Un momento, lo miro." {
		t.Errorf("without a capture: %q %v", text, err)
	}
}
