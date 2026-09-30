package orchestrator

import (
	"fmt"
	"time"
)

// silenceTimeoutTrigger is the pseudo-transcript monitorInactivity hands runLLMAndTTS when the
// caller has said nothing for SilenceTimeout. It is never a caller's words and never reaches the
// conversation history: it names why the bot is about to speak unprompted.
const silenceTimeoutTrigger = "[USER_SILENCE_TIMEOUT]"

// responseTriggerFor names what started a response, from the transcript runLLMAndTTS was given:
// "" for a caller's turn, otherwise the reason the bot spoke unprompted. The opening line is the
// only caller of runLLMAndTTS with an empty transcript — processUtterance never gets that far with
// nothing transcribed.
func responseTriggerFor(transcript string) string {
	switch transcript {
	case "":
		return "opening"
	case silenceTimeoutTrigger:
		return "silence_timeout"
	}
	return ""
}

// silenceCheckInInstruction is what the model is told when the silence nudge asks it to speak.
//
// The nudge used to send the model the conversation exactly as it stood — nothing new in it, the
// last message the agent's own reply — and leave it to guess why it was being asked again. It
// guessed "answer the caller's last question again". Traced from production on 2026-09-28
// (session agent_1790600579818180595, landing demo, Spanish): the caller asked for a restaurant
// booking, was told at 33.9 s that the agent does not take bookings, stayed quiet, and at 52.3 s
// heard the refusal again, reworded, as if it answered something new. Replayed against the
// production model (gpt-oss-120b on Cerebras) with that history, the bare re-ask re-answered the
// booking question 15 times in 32; with this instruction appended it checked in 32 times in 32
// ("¿Sigues ahí o necesitas algo más?"), never re-answering.
//
// The instruction the orchestrator's own CLI agent put in its system prompt for this marker never
// reached production prompts, and the marker itself was never in the messages the model was sent.
//
// A user-role note, not a system message: the Anthropic and Gemini providers lift every system
// message into the system prompt, which would leave the conversation ending on the agent's own
// reply — a prefill Anthropic continues rather than a turn it answers. English like the rest of the
// composed system prompt; the pinned-language section already governs the language of the reply.
func silenceCheckInInstruction(timeout time.Duration) string {
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("[Note from the system, not words the caller said: the caller has said nothing "+
		"for %d seconds since your last reply. Do not repeat, rephrase or add to your last reply, and "+
		"do not answer any earlier question again. In one short sentence, check whether they are still "+
		"there or would like anything else.]", secs)
}

// callerAwaitingReply reports whether the conversation ends on the caller's turn: their last words
// have no reply in context, because it was abandoned before any of it played or its turn was
// cancelled. System messages (the pinned call summary, injected knowledge) are not a reply.
func callerAwaitingReply(msgs []Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "system":
			continue
		case "user":
			return true
		default:
			return false
		}
	}
	return false
}

// llmMessages is what the model is sent to produce the response runLLMAndTTS was asked for: the
// conversation, plus — for the silence nudge, when the caller has been answered — the note saying
// why it is being asked to speak. The note goes on this request's copy and never into the session:
// it is not something the caller said, and a stored one would be the "last user turn" the
// continuation merge rewrites (reviseLastUserTurn).
//
// No note when the caller is still waiting for a reply. That is the other thing the nudge is for — a
// turn whose reply was discarded or cancelled leaves the caller in silence, and monitorInactivity is
// the net that catches it — and there the conversation already ends on their words, so the model
// answers them, which is right. Telling it "do not answer any earlier question" would leave them
// unanswered twice.
func (ms *ManagedStream) llmMessages(transcript string) []Message {
	msgs := ms.session.GetContextCopy()
	if transcript == silenceTimeoutTrigger && !callerAwaitingReply(msgs) {
		timeout := 10 * time.Second
		if ms.orch != nil && ms.orch.config.SilenceTimeout > 0 {
			timeout = ms.orch.config.SilenceTimeout
		}
		msgs = append(msgs, Message{Role: "user", Content: silenceCheckInInstruction(timeout)})
	}
	return msgs
}

// logBotInitiatedLatency is logTurnLatency's line for a response nobody asked for — the opening
// line, the silence nudge. There is no caller turn to measure from, so it carries only the stages
// this response went through, and the tokens it cost: those are real spend, and dropping the line
// rather than renaming it would under-count the model bill.
//
// It exists because the nudge used to be logged as a caller turn. Every caller-anchored clock still
// held the PREVIOUS turn's values, so the line read as one turn with a 10-25 second turn_gate_ms /
// gate_other_ms / ck_pre_llm_ms and every checkpoint at zero: the reply's playback, plus the silence
// timeout, plus up to one 2 s monitor tick, measured from the last turn's sttEnd. 2026-09-28's
// session logged ck_pre_llm_ms 18716 = 7.2 s of playback + 10 s + 1.0 s to the tick. That "stall"
// never happened, and several checkpoints were added to find it.
//
// A separate message rather than "turn latency" with a flag, so that "turn latency" keeps meaning
// exactly one caller turn.
func (ms *ManagedStream) logBotInitiatedLatency(trigger string) {
	first := ms.ttsFirstChunkTime
	if first.IsZero() {
		return
	}
	ms.mu.Lock()
	turnTokens := ms.turnTokens
	ms.mu.Unlock()
	response, llm := stageMs(ms.llmStartTime, first), stageMs(ms.llmStartTime, ms.llmEndTime)
	llmToTTS, ttsFirst := stageMs(ms.llmEndTime, ms.ttsStartTime), stageMs(ms.ttsStartTime, first)
	whenTokensSettled(turnTokens, func() {
		promptTok, completionTok, totalTok := -1, -1, -1
		if p, c, t, ok := turnTokens.Snapshot(); ok {
			promptTok, completionTok, totalTok = p, c, t
		}
		ms.logger.Info("bot-initiated response latency",
			"trigger", trigger,
			// llmStart -> first audio out: the whole time this response took to be heard.
			"response_ms", response,
			"llm_ms", llm,
			"llm_prompt_tokens", promptTok,
			"llm_completion_tokens", completionTok,
			"llm_total_tokens", totalTok,
			"llm_to_tts_ms", llmToTTS,
			"tts_first_chunk_ms", ttsFirst,
		)
	})
}

// toolsOffered is the tools the model may call when generating for transcript: none for the silence
// nudge. A check-in has nothing to act on, and without an instruction the nudge used to act on the
// agent's own last offer: "Want me to add it to your cart?" followed by silence became an add, and
// "Wanna book it?" a booking under "John Doe" (Full-Duplex-Bench v3, 2026-09-29). The note in
// llmMessages is what makes it check in (12 of 12 on Cerebras, tools offered or not); this makes sure
// that, whatever it writes, nothing is done on the caller's behalf while they are silent.
func (ms *ManagedStream) toolsOffered(transcript string) []Tool {
	if transcript == silenceTimeoutTrigger {
		return nil
	}
	return ms.session.GetTools()
}
