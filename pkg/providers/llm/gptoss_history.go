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
	// GptOssToolHistoryBeforeLastUser, the default, sends them together as one system message just
	// before the caller's latest message.
	GptOssToolHistoryBeforeLastUser GptOssToolHistory = "tail"
	// GptOssToolHistoryNotes sends each as a system message where it happened, as from 2026-09-29 to
	// 2026-09-30. Kept to compare against on chosen agents.
	GptOssToolHistoryNotes GptOssToolHistory = "notes"
)

// defaultGptOssToolHistory is what a provider uses when nothing was set.
const defaultGptOssToolHistory = GptOssToolHistoryBeforeLastUser

// earlierToolsHeading opens the system message that gathers the earlier exchanges.
const earlierToolsHeading = "# Tools called earlier in this call"

// ParseGptOssToolHistory reads a GptOssToolHistory by name; ok is false for anything else.
func ParseGptOssToolHistory(s string) (h GptOssToolHistory, ok bool) {
	switch h := GptOssToolHistory(strings.ToLower(strings.TrimSpace(s))); h {
	case GptOssToolHistoryNotes, GptOssToolHistoryBeforeLastUser:
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
// and the lines go together in one system message just before that message. The current turn's
// exchanges, which the model is answering, are left as they are.
//
// gpt-oss writes a second call to a tool it has already called in the conversation as text instead
// of making it: the arguments as JSON, "to=functions.add_to_cart", an invented result, all of which
// the provider returns as the reply, and the call is never made. Ten of a hundred Full-Duplex-Bench
// calls hit it on 2026-09-29, mostly "add it to my cart" a second time. Measured on Cerebras with a
// clean history, a second add_to_cart was made 2 times in 12 (10 written out); with the earlier
// exchange as a note, 12 in 12 (none written out). reasoning_effort "medium" also fixes it, but
// doubles the time to the first spoken token on every turn.
//
// Where the lines go matters as much. Each as a system message where its exchange happened (notes,
// the first version) made gpt-oss garble the reply after a later tool call: a word said twice, then
// runs of ellipses, no-break spaces and its own reasoning ("Tu cita cita c…?", "Oops. Need to respond
// correctly."), all of it spoken. Scripted calls on Cerebras through this code, 2026-09-30 (lokutor_tts
// pkg/api/gptoss_history_live_test.go): a clinic booking (check_availability twice, then
// book_appointment) garbled in 30 of 32 calls with notes and 0 of 12 with one message here; a shop call
// adding to the cart three times, 4 of 12 and 0 of 12. The repeated call was made every time either way
// (24 of 24). Full-Duplex-Bench through production the same day, 100 calls each at the same time
// (Gemini 3 Flash judge): pass 48% against 47%, tool selection 84.4% against 79.2%, response quality 65%
// against 64%, more tool calls than asked for 19 against 22; the model's time on a turn unchanged (p50
// 265 ms against 271). Folding the lines into the end of the system prompt also stopped the garbling
// (2 of 22 clinic calls, one doubled word each) but, told about the calls away from where they
// happened, the model repeated and wrote out more of them: 29 and 26 extra calls against notes' 23 and
// 22 in two benchmark runs, 4 written out against 1.
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
	var gathered []string
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
			if where == GptOssToolHistoryNotes {
				out = append(out, orchestrator.Message{Role: "system", Content: note})
			} else {
				gathered = append(gathered, note)
			}
		default:
			out = append(out, m)
		}
	}
	if len(gathered) == 0 {
		return out
	}
	section := orchestrator.Message{Role: "system", Content: earlierToolsHeading + "\n" + strings.Join(gathered, "\n")}
	return append(out[:lastUserAt:lastUserAt], append([]orchestrator.Message{section}, out[lastUserAt:]...)...)
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
