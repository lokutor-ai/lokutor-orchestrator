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

func gptOssHistory() []orchestrator.Message {
	return []orchestrator.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "add it"},
		tcMsg("c1", "add_to_cart", `{"product_id":"PROD1"}`),
		{Role: "tool", Content: `{"status":"success"}`, ToolCallID: "c1", Name: "add_to_cart"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "what's in it"},
		tcMsg("c2", "view_cart", `{}`),
		{Role: "tool", Content: `{"items":1}`, ToolCallID: "c2", Name: "view_cart"},
		{Role: "assistant", Content: "One item."},
		{Role: "user", Content: "add another"},
		tcMsg("c3", "add_to_cart", `{"product_id":"PROD1"}`),
		{Role: "tool", Content: `{"status":"success"}`, ToolCallID: "c3", Name: "add_to_cart"},
	}
}

func TestForGptOssNotes(t *testing.T) {
	history := gptOssHistory()
	want := []orchestrator.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "add it"},
		{Role: "system", Content: `Tool add_to_cart was called with {"product_id":"PROD1"} and returned {"status":"success"}`},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "what's in it"},
		{Role: "system", Content: `Tool view_cart was called with {} and returned {"items":1}`},
		{Role: "assistant", Content: "One item."},
		// The turn being answered keeps its exchange as it is.
		{Role: "user", Content: "add another"},
		history[10],
		history[11],
	}
	assert.Equal(t, want, forGptOss("gpt-oss-120b", history, GptOssToolHistoryNotes))
	assert.Equal(t, want, forGptOss("gpt-oss-120b", history, ""), "notes is the default")

	assert.Equal(t, history, forGptOss("llama-3.3-70b", history, GptOssToolHistoryNotes), "other models are sent the history unchanged")
	current := history[9:]
	assert.Equal(t, current, forGptOss("openai/gpt-oss-120b", current, GptOssToolHistoryInPrompt), "nothing earlier, nothing to rewrite")
}

func TestForGptOssInPrompt(t *testing.T) {
	history := gptOssHistory()
	got := forGptOss("gpt-oss-120b", history, GptOssToolHistoryInPrompt)
	assert.Equal(t, []orchestrator.Message{
		{Role: "system", Content: "prompt\n\n# Tools called earlier in this call\n" +
			`Tool add_to_cart was called with {"product_id":"PROD1"} and returned {"status":"success"}` + "\n" +
			`Tool view_cart was called with {} and returned {"items":1}`},
		{Role: "user", Content: "add it"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "what's in it"},
		{Role: "assistant", Content: "One item."},
		{Role: "user", Content: "add another"},
		history[10],
		history[11],
	}, got)
	assert.Equal(t, "prompt", history[0].Content, "the session's system prompt is not modified")

	noPrompt := history[1:]
	got = forGptOss("gpt-oss-120b", noPrompt, GptOssToolHistoryInPrompt)
	assert.Equal(t, "system", got[0].Role, "with no system message, the section is one")
	assert.Equal(t, "add it", got[1].Content)
}

func TestSetGptOssToolHistory(t *testing.T) {
	c, g, o := NewCerebrasLLM("k", ""), NewGroqLLM("k", ""), NewOpenRouterLLM("k", "", nil, "")
	SetGptOssToolHistory(NewChainLLM("chain", c, NewChainLLM("inner", g, o)), GptOssToolHistoryInPrompt)
	assert.Equal(t, GptOssToolHistoryInPrompt, c.toolHistory)
	assert.Equal(t, GptOssToolHistoryInPrompt, g.toolHistory)
	assert.Equal(t, GptOssToolHistoryInPrompt, o.toolHistory)

	h, ok := ParseGptOssToolHistory(" Prompt ")
	assert.True(t, ok)
	assert.Equal(t, GptOssToolHistoryInPrompt, h)
	_, ok = ParseGptOssToolHistory("top")
	assert.False(t, ok)
}
