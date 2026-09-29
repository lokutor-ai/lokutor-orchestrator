package orchestrator

import (
	"strings"
	"testing"
	"time"
)

func TestResponseTriggerFor(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "opening",
		silenceTimeoutTrigger:         "silence_timeout",
		"¿A qué hora abre mañana?":    "",
		"[user_silence_timeout]":      "", // exact match only
		"what are your opening hours": "",
	} {
		if got := responseTriggerFor(in); got != want {
			t.Errorf("responseTriggerFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// The clocks production logged on 2026-09-28: the caller's last turn ended ~18.7 s before the nudge
// started. As a caller turn that is a 18,716 ms ck_pre_llm_ms; as what it is, a 300 ms response.
func TestSilenceNudgeIsNotLoggedAsACallerTurn(t *testing.T) {
	log := &replayLog{}
	ms := &ManagedStream{logger: log}
	now := time.Now()
	ms.lastVoicedAt = now.Add(-19193 * time.Millisecond)
	ms.userSpeechEnd = now.Add(-19023 * time.Millisecond)
	ms.sttStartTime = ms.userSpeechEnd
	ms.sttEndTime = ms.userSpeechEnd
	ms.llmStartTime = now.Add(-304 * time.Millisecond)
	ms.llmEndTime = now.Add(-67 * time.Millisecond)
	ms.ttsStartTime = now.Add(-62 * time.Millisecond)
	ms.ttsFirstChunkTime = now
	ms.responseTrigger = responseTriggerFor(silenceTimeoutTrigger)

	ms.logTurnLatency()

	if got := log.find("turn latency"); len(got) != 0 {
		t.Fatalf("the nudge was logged as a caller turn: %+v", got[0].kv)
	}
	bot := log.find("bot-initiated response latency")
	if len(bot) != 1 || bot[0].kv["trigger"] != "silence_timeout" {
		t.Fatalf("want one silence_timeout line, got %+v", bot)
	}
	if v, _ := bot[0].kv["response_ms"].(int64); v < 295 || v > 315 {
		t.Errorf("response_ms = %v, want ~304 (llm start -> first audio)", bot[0].kv["response_ms"])
	}
	for _, k := range []string{"llm_prompt_tokens", "llm_completion_tokens", "llm_total_tokens"} {
		if _, ok := bot[0].kv[k]; !ok {
			t.Errorf("%s missing: the nudge's tokens are real spend", k)
		}
	}

	// The same clocks on a caller's turn are a (slow) caller turn, and still logged as one.
	ms.responseTrigger = ""
	ms.logTurnLatency()
	if got := log.find("turn latency"); len(got) != 1 {
		t.Fatalf("a caller turn must still produce its turn latency line, got %d", len(got))
	}
}

func TestCallerAwaitingReply(t *testing.T) {
	sys := Message{Role: "system", Content: "prompt"}
	u := Message{Role: "user", Content: "¿Puedes reservarme la cita?"}
	a := Message{Role: "assistant", Content: "No gestiono reservas."}
	for _, tc := range []struct {
		name string
		msgs []Message
		want bool
	}{
		{"empty", nil, false},
		{"only the system prompt", []Message{sys}, false},
		{"answered", []Message{sys, u, a}, false},
		{"reply discarded", []Message{sys, a, u}, true},
		{"summary after the question is not an answer", []Message{sys, u, sys}, true},
		{"tool result last", []Message{sys, u, {Role: "tool", Content: "{}"}}, false},
	} {
		if got := callerAwaitingReply(tc.msgs); got != tc.want {
			t.Errorf("%s: callerAwaitingReply = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLLMMessagesAddsTheCheckInNoteOnlyToTheNudge(t *testing.T) {
	ms := newTruthStream(t) // SilenceTimeout 0 here: the note falls back to production's 10 s
	ms.session.AddMessage("user", "En el restaurante a las nueve.")
	ms.session.AddMessage("assistant", "Lo siento, no gestiono reservas.")
	stored := len(ms.session.GetContextCopy())

	got := ms.llmMessages(silenceTimeoutTrigger)
	if len(got) != stored+1 {
		t.Fatalf("want the conversation plus one note, got %d messages for %d stored", len(got), stored)
	}
	note := got[len(got)-1]
	if note.Role != "user" || !strings.Contains(note.Content, "said nothing for 10 seconds") ||
		!strings.Contains(note.Content, "Do not repeat") {
		t.Fatalf("note = %+v", note)
	}
	if n := len(ms.session.GetContextCopy()); n != stored {
		t.Fatalf("the note was stored in the conversation (%d messages, want %d)", n, stored)
	}

	// A caller's turn goes to the model as the conversation, nothing added.
	if got := ms.llmMessages("¿Y el horario?"); len(got) != stored {
		t.Fatalf("a caller turn got %d messages, want %d", len(got), stored)
	}

	// A caller still waiting for a reply gets it answered: no "do not answer" note.
	ms.session.AddMessage("user", "¿Y mañana?")
	got = ms.llmMessages(silenceTimeoutTrigger)
	if last := got[len(got)-1]; last.Role != "user" || last.Content != "¿Y mañana?" {
		t.Fatalf("the nudge for an unanswered caller must end on their words, got %+v", last)
	}
}

// A speculative hit and a non-streaming model record the reply before speaking it. When the caller
// resumed before any of it played, the record is taken back: out of context and off the transcript.
func TestWithdrawUnheardReply(t *testing.T) {
	ms := newTruthStream(t)
	const reply = "Claro, dime el día. ¿Para cuántas personas?"
	ms.session.AddMessage("user", "¿Puedes reservarme la cita?")
	ms.session.AddMessage("assistant", reply)
	ms.mu.Lock()
	ms.resumeDiscardGen = 5
	ms.mu.Unlock()

	ms.withdrawUnheardReply(5, reply)

	for _, m := range ms.session.GetContextCopy() {
		if m.Content == reply {
			t.Fatal("the unheard reply is still in context")
		}
	}
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ms.Events():
			if ev.Type != BotResponseTruncated {
				continue
			}
			tr, _ := ev.Data.(TruncatedResponse)
			if tr.Full != reply || tr.Spoken != "" || !tr.Emitted {
				t.Fatalf("truncation = %+v, want the reply withdrawn (Spoken empty, Emitted)", tr)
			}
			return
		case <-deadline:
			t.Fatal("no BotResponseTruncated: the transcript keeps a line the caller never heard")
		}
	}
}

// Audio of the reply reached the transport: something was heard, and what was heard is settled by
// the interruption path, not withdrawn here.
func TestWithdrawUnheardReplyKeepsAReplyThatStartedPlaying(t *testing.T) {
	ms := newTruthStream(t)
	const reply = "Claro. ¿Para cuántas personas?"
	ms.session.AddMessage("user", "¿Puedes reservarme la cita?")
	ms.session.AddMessage("assistant", reply)
	ms.mu.Lock()
	ms.beginSegmentLocked(6, "Claro.")
	ms.scheduleAudioLocked(6, 3200, time.Now())
	ms.resumeDiscardGen = 6 // the second sentence was abandoned
	ms.mu.Unlock()

	ms.withdrawUnheardReply(6, reply)

	ctx := ms.session.GetContextCopy()
	if last := ctx[len(ctx)-1]; last.Content != reply {
		t.Fatalf("a reply that started playing was withdrawn: last = %+v", last)
	}
}

// The nudge is offered no tools: a check-in must never act on the agent's own last offer.
func TestSilenceNudgeIsOfferedNoTools(t *testing.T) {
	session := NewConversationSession("nudge-tools")
	session.SetTools([]Tool{{Type: "function", Function: map[string]interface{}{"name": "book_flight"}}})
	ms := &ManagedStream{session: session}
	if got := ms.toolsOffered(silenceTimeoutTrigger); got != nil {
		t.Fatalf("the silence nudge was offered tools: %v", got)
	}
	if got := ms.toolsOffered("book it please"); len(got) != 1 {
		t.Fatalf("a caller's turn must keep its tools, got %v", got)
	}
}
