package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// forGptOss returns the history as a gpt-oss model should be sent it: every tool exchange from
// before the caller's latest message becomes a system note of what was called and what it
// returned. The current turn's exchanges, which the model is answering, are left as they are.
//
// gpt-oss writes a second call to a tool it has already called in the conversation as text instead
// of making it: the arguments as JSON, "to=functions.add_to_cart", an invented result, all of which
// the provider returns as the reply, and the call is never made. Ten of a hundred Full-Duplex-Bench
// calls hit it on 2026-09-29, mostly "add it to my cart" a second time. Measured on Cerebras with a
// clean history, a second add_to_cart was made 2 times in 12 (10 written out); with the earlier
// exchange as a note, 12 in 12 (none written out). reasoning_effort "medium" also fixes it, but
// doubles the time to the first spoken token on every turn.
func forGptOss(model string, messages []orchestrator.Message) []orchestrator.Message {
	if !strings.Contains(strings.ToLower(model), "gpt-oss") {
		return messages
	}
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = i
			break
		}
	}
	earlier := false
	for _, m := range messages[:max(lastUser, 0)] {
		if m.Role == "tool" || m.ToolCalls != nil {
			earlier = true
			break
		}
	}
	if !earlier {
		return messages
	}

	type call struct{ name, args string }
	calls := map[string]call{}
	out := make([]orchestrator.Message, 0, len(messages))
	for i, m := range messages {
		switch {
		case i >= lastUser:
			out = append(out, m)
		case m.ToolCalls != nil:
			for _, c := range toolCallsIn(m.ToolCalls) {
				calls[c.ID] = call{c.Function.Name, c.Function.Arguments}
			}
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, orchestrator.Message{Role: m.Role, Content: m.Content})
			}
		case m.Role == "tool":
			c := calls[m.ToolCallID]
			if c.name == "" {
				c.name = m.Name
			}
			out = append(out, orchestrator.Message{Role: "system",
				Content: fmt.Sprintf("Tool %s was called with %s and returned %s", c.name, c.args, m.Content)})
		default:
			out = append(out, m)
		}
	}
	return out
}

type toolCallRecord struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallsIn reads a message's ToolCalls whatever shape it was built in.
func toolCallsIn(v interface{}) []toolCallRecord {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var calls []toolCallRecord
	if json.Unmarshal(raw, &calls) != nil {
		return nil
	}
	return calls
}
