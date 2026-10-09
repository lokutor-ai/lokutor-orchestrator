package orchestrator

import (
	"encoding/json"
	"time"
)

// Farewell ends the call from the agent's side, gracefully: once the agent has finished what it is
// saying, the model is told why the call must end (note), says a short goodbye in the conversation's
// language, and is offered only end_call, so the host's normal end_call handling hangs up after the
// goodbye has played. The host uses it when the account behind the call can no longer pay for it.
//
// It waits up to wait for the stream to be between turns (not thinking, not speaking) so it never cuts
// the agent off; past that it goes ahead anyway. It returns false if the stream is already closed. The
// host must still hang up on its own after a grace period: a model that does not call end_call, or an
// agent with no end_call tool, would otherwise keep the line open.
func (ms *ManagedStream) Farewell(note string, wait time.Duration) bool {
	if ms == nil || ms.ctx.Err() != nil {
		return false
	}
	ms.mu.Lock()
	ms.farewellNote = note
	ms.mu.Unlock()
	go func() {
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			if ms.ctx.Err() != nil {
				return
			}
			ms.mu.Lock()
			between := ms.state == StateIdle || ms.state == StateListening || ms.state == StateInterrupted
			ms.mu.Unlock()
			if between {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if ms.ctx.Err() != nil {
			return
		}
		ms.logger.Info("Host is ending the call: agent saying goodbye", "note", note)
		ms.runLLMAndTTS(ms.ctx, farewellTrigger)
	}()
	return true
}

// toolFunctionName is a tool's function name, whatever shape its Function field was built with (a
// map from JSON, or a struct with a Name field).
func toolFunctionName(t Tool) string {
	if m, ok := t.Function.(map[string]interface{}); ok {
		name, _ := m["name"].(string)
		return name
	}
	raw, err := json.Marshal(t.Function)
	if err != nil {
		return ""
	}
	var f struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &f)
	return f.Name
}
