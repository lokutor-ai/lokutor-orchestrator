package llm

import (
	"testing"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	"github.com/stretchr/testify/assert"
)

func tcMsg(id, name, args string) orchestrator.Message {
	return orchestrator.Message{Role: "assistant", ToolCalls: []interface{}{map[string]interface{}{
		"id": id, "type": "function", "function": map[string]interface{}{"name": name, "arguments": args}}}}
}

func TestForGptOss(t *testing.T) {
	history := []orchestrator.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "add it"},
		tcMsg("c1", "add_to_cart", `{"product_id":"PROD1"}`),
		{Role: "tool", Content: `{"status":"success"}`, ToolCallID: "c1", Name: "add_to_cart"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "add another"},
		tcMsg("c2", "add_to_cart", `{"product_id":"PROD1"}`),
		{Role: "tool", Content: `{"status":"success"}`, ToolCallID: "c2", Name: "add_to_cart"},
	}
	got := forGptOss("gpt-oss-120b", history)
	assert.Equal(t, []orchestrator.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "add it"},
		{Role: "system", Content: `Tool add_to_cart was called with {"product_id":"PROD1"} and returned {"status":"success"}`},
		{Role: "assistant", Content: "Done."},
		// The turn being answered keeps its exchange as it is.
		{Role: "user", Content: "add another"},
		history[6],
		history[7],
	}, got)

	assert.Equal(t, history, forGptOss("llama-3.3-70b", history), "other models are sent the history unchanged")
	current := history[5:]
	assert.Equal(t, current, forGptOss("openai/gpt-oss-120b", current), "nothing earlier, nothing to rewrite")
}
