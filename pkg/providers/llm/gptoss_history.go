package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	orchestrator "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// GptOssToolHistory is where forGptOss puts the tool exchanges from before the caller's latest
// message.
type GptOssToolHistory string

const (
	// GptOssToolHistoryNotes sends each earlier exchange as a system message where it happened.
	GptOssToolHistoryNotes GptOssToolHistory = "notes"
	// GptOssToolHistoryInPrompt sends them together as one section at the end of the first system
	// message.
	GptOssToolHistoryInPrompt GptOssToolHistory = "prompt"
	// GptOssToolHistoryBeforeLastUser sends them together as one system message just before the
	// caller's latest message.
	GptOssToolHistoryBeforeLastUser GptOssToolHistory = "tail"
)

// defaultGptOssToolHistory is what a provider uses when nothing was set.
const defaultGptOssToolHistory = GptOssToolHistoryNotes

// earlierToolsHeading opens the section GptOssToolHistoryInPrompt adds to the system prompt.
const earlierToolsHeading = "# Tools called earlier in this call"

// ParseGptOssToolHistory reads a GptOssToolHistory by name; ok is false for anything else.
func ParseGptOssToolHistory(s string) (h GptOssToolHistory, ok bool) {
	switch h := GptOssToolHistory(strings.ToLower(strings.TrimSpace(s))); h {
	case GptOssToolHistoryNotes, GptOssToolHistoryInPrompt, GptOssToolHistoryBeforeLastUser:
		return h, true
	}
	return "", false
}

// SetGptOssToolHistory sets where p, a provider or a chain of them, puts a gpt-oss model's earlier
// tool exchanges. Call it before the provider is used. Providers that never send a gpt-oss model
// ignore it.
func SetGptOssToolHistory(p orchestrator.LLMProvider, h GptOssToolHistory) {
	switch v := p.(type) {
	case *ChainLLM:
		for _, c := range v.providers {
			SetGptOssToolHistory(c, h)
		}
	case *CerebrasLLM:
		v.toolHistory = h
	case *GroqLLM:
		v.toolHistory = h
	case *OpenRouterLLM:
		v.toolHistory = h
	}
}

// forGptOss returns the history as a gpt-oss model should be sent it: every tool exchange from
// before the caller's latest message becomes a line saying what was called and what it returned,
// placed as where says. The current turn's exchanges, which the model is answering, are left as
// they are.
//
// gpt-oss writes a second call to a tool it has already called in the conversation as text instead
// of making it: the arguments as JSON, "to=functions.add_to_cart", an invented result, all of which
// the provider returns as the reply, and the call is never made. Ten of a hundred Full-Duplex-Bench
// calls hit it on 2026-09-29, mostly "add it to my cart" a second time. Measured on Cerebras with a
// clean history, a second add_to_cart was made 2 times in 12 (10 written out); with the earlier
// exchange as a note, 12 in 12 (none written out). reasoning_effort "medium" also fixes it, but
// doubles the time to the first spoken token on every turn.
func forGptOss(model string, messages []orchestrator.Message, where GptOssToolHistory) []orchestrator.Message {
	if !strings.Contains(strings.ToLower(model), "gpt-oss") {
		return messages
	}
	if where == "" {
		where = defaultGptOssToolHistory
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
	var folded []string
	lastUserAt := 0
	out := make([]orchestrator.Message, 0, len(messages)+1)
	for i, m := range messages {
		switch {
		case i >= lastUser:
			if i == lastUser {
				lastUserAt = len(out)
			}
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
			note := fmt.Sprintf("Tool %s was called with %s and returned %s", c.name, c.args, m.Content)
			if where == GptOssToolHistoryInPrompt || where == GptOssToolHistoryBeforeLastUser {
				folded = append(folded, note)
			} else {
				out = append(out, orchestrator.Message{Role: "system", Content: note})
			}
		default:
			out = append(out, m)
		}
	}
	if len(folded) > 0 {
		section := earlierToolsHeading + "\n" + strings.Join(folded, "\n")
		if where == GptOssToolHistoryBeforeLastUser {
			note := orchestrator.Message{Role: "system", Content: section}
			return append(out[:lastUserAt:lastUserAt], append([]orchestrator.Message{note}, out[lastUserAt:]...)...)
		}
		if len(out) > 0 && out[0].Role == "system" {
			// out[0] is a copy: the session's own system prompt is not touched.
			out[0].Content = strings.TrimRight(out[0].Content, "\n") + "\n\n" + section
		} else {
			out = append([]orchestrator.Message{{Role: "system", Content: section}}, out...)
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
