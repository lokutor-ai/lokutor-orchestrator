package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	"github.com/stretchr/testify/assert"
)

func sseServer(lines ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintln(w, "data: "+l)
		}
		fmt.Fprintln(w, "data: [DONE]")
	}))
}

// A run-on as Cerebras streams it: the reply, the model's next analysis, the caller it invented, and
// a call made for that caller. The reply is what is kept; the call is not made; the tokens still
// count.
func TestCerebrasStream_CutsARunOnAndDropsItsCalls(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"reasoning":"Need to qualify."}}]}`,
		`{"choices":[{"delta":{"content":"Entiendo. ¿Cuántas llamadas recibís al día?"}}]}`,
		`{"choices":[{"delta":{"reasoning":"User hasn't responded. Need to wait."}}]}`,
		`{"choices":[{"delta":{"content":"Claro, soy Juan Pérez, del taller Pérez."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"book_appointment","arguments":"{\"caller_name\":\"Juan Pérez\"}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":4593,"completion_tokens":195,"total_tokens":4788}}`,
	)
	defer ts.Close()
	l := NewCerebrasLLM("k", "gpt-oss-120b")
	l.url = ts.URL
	u := &orchestrator.TokenUsage{}
	ctx := orchestrator.WithTokenUsage(context.Background(), u)

	var streamed string
	var calls []orchestrator.ToolCallEventData
	got, err := l.StreamComplete(ctx, nil, nil,
		func(s string) error { streamed += s; return nil },
		func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	assert.NoError(t, err)
	assert.Equal(t, "Entiendo. ¿Cuántas llamadas recibís al día?", got)
	assert.Equal(t, got, streamed)
	assert.Empty(t, calls, "a call made for an imagined caller is not made")

	assert.Eventually(t, func() bool { _, _, _, ok := u.Snapshot(); return ok }, time.Second, 10*time.Millisecond,
		"the rest of the stream is still read for its token counts")
	cuts := u.TakeRunOns()
	if assert.Len(t, cuts, 1) {
		assert.Eventually(t, func() bool { return cuts[0].Dropped() == "Claro, soy Juan Pérez, del taller Pérez." },
			time.Second, 10*time.Millisecond)
	}
}

// Words before a call, with the model's reasoning between them, are a preamble: the call is made.
func TestCerebrasStream_PreambleThenCallIsKept(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"content":"Vale, lo miro."}}]}`,
		`{"choices":[{"delta":{"reasoning":"Call check_availability."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"check_availability","arguments":"{\"date\":\"2026-10-02\"}"}}]}}]}`,
	)
	defer ts.Close()
	l := NewCerebrasLLM("k", "gpt-oss-120b")
	l.url = ts.URL
	var calls []orchestrator.ToolCallEventData
	got, err := l.StreamComplete(context.Background(), nil, nil, nil,
		func(tc orchestrator.ToolCallEventData) error { calls = append(calls, tc); return nil })
	assert.NoError(t, err)
	assert.Equal(t, "Vale, lo miro.", got)
	if assert.Len(t, calls, 1) {
		assert.Equal(t, "check_availability", calls[0].Name)
	}
}

// Only gpt-oss runs on this way; another model's text is passed as it comes.
func TestOpenRouterStream_OtherModelsPassThrough(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"content":"¿Cuántas llamadas?"}}]}`,
		`{"choices":[{"delta":{"reasoning":"x"}}]}`,
		`{"choices":[{"delta":{"content":"¿Y en qué idiomas?"}}]}`,
	)
	defer ts.Close()
	l := NewOpenRouterLLM("k", "deepseek/deepseek-v4.1-flash", nil, "")
	l.url = ts.URL
	got, err := l.StreamComplete(context.Background(), nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, "¿Cuántas llamadas?¿Y en qué idiomas?", got)
}

func TestGroqStream_GluedRunOnIsCut(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"content":"¿Cuántas llamadas recibís al día?¿Aproximadamente"}}]}`,
		`{"choices":[{"delta":{"content":" cuántas llamadas manejáis?"}}]}`,
	)
	defer ts.Close()
	l := NewGroqLLM("k", "openai/gpt-oss-120b")
	l.url = ts.URL
	got, err := l.StreamComplete(context.Background(), nil, nil, nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, "¿Cuántas llamadas recibís al día?", got)
}
