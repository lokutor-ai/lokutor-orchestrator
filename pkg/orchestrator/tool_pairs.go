package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// toolExchange is one tool call and what it returned.
type toolExchange struct {
	TC     ToolCallEventData `json:"tool_call"`
	Result string            `json:"result"`
}

// recordToolExchange appends a round of tool calls to the history the way every provider requires
// it: the assistant message carrying the calls, then one result per call, in a single append so
// nothing can land between them.
func (ms *ManagedStream) recordToolExchange(content string, calls []toolExchange) {
	if len(calls) == 0 {
		return
	}
	tcData := make([]interface{}, 0, len(calls))
	for _, c := range calls {
		tcData = append(tcData, map[string]interface{}{
			"id":   c.TC.CallID,
			"type": "function",
			"function": map[string]interface{}{
				"name":      c.TC.Name,
				"arguments": c.TC.Arguments,
			},
		})
	}
	msgs := []Message{{Role: "assistant", Content: content, ToolCalls: tcData}}
	for _, c := range calls {
		// Give the model something it can use rather than an empty tool message.
		result := strings.TrimSpace(c.Result)
		if result == "" {
			result = `{"result": "no result"}`
		} else if !strings.HasPrefix(result, "{") && !strings.HasPrefix(result, "[") {
			result = fmt.Sprintf(`{"result": %s}`, jsonQuote(result))
		}
		msgs = append(msgs, Message{Role: "tool", Content: result, ToolCallID: c.TC.CallID, Name: c.TC.Name})
	}
	ms.session.AddMessagesRaw(msgs...)
}

// pairedToolMessages returns msgs without the tool exchanges a provider would reject: a tool message
// that answers no call just before it, and the tool calls of an assistant message that are not each
// answered right after it (the calls and their partial results go; the message's words, if any,
// stay). One such message fails every request that carries it and stays in the history, so it
// silences the rest of the call, not one turn: on 2026-09-29 chained tool calls recorded their
// results without the calls, Cerebras answered 400 to every later turn, and five benchmark calls went
// quiet. dropped counts the messages removed.
func pairedToolMessages(msgs []Message) (out []Message, dropped int) {
	out = make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == "tool" {
			// A result that answers a call was taken with the call, below.
			dropped++
			i++
			continue
		}
		i++
		if m.ToolCalls == nil {
			out = append(out, m)
			continue
		}
		j := i
		for j < len(msgs) && msgs[j].Role == "tool" {
			j++
		}
		ids, ok := toolCallIDs(m.ToolCalls)
		if !ok {
			// A shape this cannot read is left alone: it only removes what it can show is broken.
			out = append(out, msgs[i-1:j]...)
			i = j
			continue
		}
		want := make(map[string]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
		var results []Message
		for _, r := range msgs[i:j] {
			if want[r.ToolCallID] {
				results = append(results, r)
				delete(want, r.ToolCallID)
			} else {
				dropped++
			}
		}
		if len(ids) > 0 && len(want) == 0 {
			out = append(out, m)
			out = append(out, results...)
		} else {
			dropped += len(results)
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, Message{Role: m.Role, Content: m.Content, Name: m.Name})
			} else {
				dropped++
			}
		}
		i = j
	}
	return out, dropped
}

// toolCallIDs reads the call ids out of a message's ToolCalls, whatever shape it was built in.
func toolCallIDs(toolCalls interface{}) ([]string, bool) {
	raw, err := json.Marshal(toolCalls)
	if err != nil {
		return nil, false
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return nil, false
	}
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ID
	}
	return ids, true
}

// toolCallInSpeech reports whether text is a tool call written out as words instead of made: it
// names one of the session's tools that has an underscore (never a spoken word) or holds a JSON
// object. gpt-oss does this now and then, and the text is otherwise read aloud: on 2026-09-29 a
// benchmark caller heard "tool search apartments arguments bedrooms city Atlanta max price".
func toolCallInSpeech(text string, tools []Tool) bool {
	if strings.Contains(text, `{"`) || strings.Contains(text, `":`) {
		return true
	}
	lower := strings.ToLower(text)
	for _, t := range tools {
		raw, err := json.Marshal(t.Function)
		if err != nil {
			continue
		}
		var fn struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &fn) == nil && strings.Contains(fn.Name, "_") && strings.Contains(lower, strings.ToLower(fn.Name)) {
			return true
		}
	}
	return false
}

// reasoningInReply matches gpt-oss's reasoning when it leaks into a reply. It talks about the call it
// has to make and about the caller in the third person, which a reply to the caller never does: "Add
// it.We should call add_to_cart.We need to call add_to_cart now.Let's call." and "The assistant should
// answer with:" (2026-09-29). Sentences run together with no space are NOT the sign: gpt-oss joins its
// own messages that way in good replies too ("Sorry, I'm not sure what you meant.What's your budget?"),
// and a rule on that alone threw away fine answers.
var reasoningInReply = regexp.MustCompile(`(?i)\b(we (need|should|must|have) to (call|invoke|execute|format)|we should call|let's (call|invoke)|now (call|execute)\b|need to call|the user (says|said|wants|asked|seems|is asking)|user says|the assistant (should|must|needs to|will)|call the tool)`)

// unspeakableReply says why text must not be read to the caller, or "" if it can be: a tool call
// written out instead of made (toolCallInSpeech), or the model's reasoning leaking into its reply.
// gpt-oss does both when its tool call comes out malformed and the provider cannot parse it as one:
// the call is then never made, and without this the caller heard "product id prod quantity one".
func unspeakableReply(text string, tools []Tool) string {
	if toolCallInSpeech(text, tools) {
		return "tool call written out"
	}
	if reasoningInReply.MatchString(text) {
		return "model reasoning"
	}
	return ""
}
