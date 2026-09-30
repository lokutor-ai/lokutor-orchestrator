package orchestrator

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// gpt-oss goes on past its turn. Asked for one reply, it writes the reply, ends the message, and then
// writes more messages in the same response: another question, the caller's answer, a tool call as
// text, its own thoughts about the caller not having answered yet. The providers return every final
// message's text as `content`, one after another with nothing between them, so what reached the
// caller on 2026-09-30 (Lucía, telnyx_1790766087241951978) was "Entiendo. ¿Cuántas llamadas recibís
// al día aproximadamente?¿Y en qué idiomas…?¿Podrías darme tu nombre…?Claro, soy Juan Pérez, del
// taller Pérez, gerente…{"tool": "check_availability"…}" — questions glued together, then the caller
// invented, then a call written out. The next turn the model saw its own glued questions as the
// agent's line and did it again.
//
// Replaying that call's requests against Cerebras (token for token: 4,443 and 4,593 prompt tokens as
// logged; pkg/api/gptoss_runon_live_test.go in lokutor_tts) ran on in 14 of 30 replies. The stream
// shows where each message ends in one of two ways:
//   - reasoning again after content ("Now ask volume.", "User hasn't responded. Need to wait."): a
//     new analysis message, then a new final one. A chunk can carry the end of one and the start of
//     the other, in either order.
//   - text glued to a sentence end with no space ("?¿", "?Para", "?(Esperando respuesta…)",
//     "tarde.¿Te"): a final message straight after another, with no reasoning between them. The
//     model does not write that inside one message.
//
// ReplyTurn keeps the reply to the turn the model was asked for. A question hands the floor to the
// caller, so the reply ends at the first message boundary after one; before a question, one more
// message is kept ("Perdona, no te he entendido. ¿Qué presupuesto tienes?"), because dropping a good
// follow-up is what made an earlier rule on glued sentences worse than none (reasoningInReply). The
// rest is dropped, and so are tool calls made after it: they act on a caller the model imagined.
type ReplyTurn struct {
	ctx context.Context
	// kept is the reply so far; last and lastNonSpace are its last runes.
	kept         strings.Builder
	last         rune
	lastNonSpace rune
	// lowerRun counts the lowercase letters just before last, for "meant.What".
	lowerRun, lowerRunAtDot int
	// boundary: reasoning came after content, so the next content starts another message.
	boundary bool
	extra    int // messages kept after the first
	asked    bool
	ended    bool
	cut      *RunOnCut
}

// NewReplyTurn cuts one reply; a cut is noted on ctx's TokenUsage, if it has one. A nil *ReplyTurn
// passes everything through, for providers whose model does not run on.
func NewReplyTurn(ctx context.Context) *ReplyTurn { return &ReplyTurn{ctx: ctx} }

// OneTurn cuts a whole reply, as it comes from a non-streaming call: only the glued boundaries show
// there, since a whole reply has no reasoning between its messages.
func OneTurn(ctx context.Context, text string) (string, *RunOnCut) {
	t := NewReplyTurn(ctx)
	out := strings.TrimSpace(t.take(text))
	return out, t.cut
}

// Chunk takes one streamed chunk's reasoning and content and returns the part of the content that
// belongs to the reply, "" once the reply has ended.
func (t *ReplyTurn) Chunk(reasoning, content string) string {
	if t == nil {
		return content
	}
	if t.ended {
		t.cut.Drop(content)
		return ""
	}
	if reasoning == "" || t.lastNonSpace == 0 {
		return t.take(content)
	}
	if content == "" {
		t.boundary = true
		return ""
	}
	// Both in one chunk, and the JSON does not say which came first. A new message starts glued to
	// the last one's sentence end ("?" then "¿Cuánt"); the tail of this one completes a sentence
	// (" taller?" after "…llegan a tu") or begins with a space.
	if isSentenceEnd(t.lastNonSpace) && !unicode.IsSpace(firstRune(content)) {
		t.boundary = true
		return t.take(content)
	}
	out := t.take(content)
	if !t.ended {
		t.boundary = true
	}
	return out
}

// Ended reports that the reply is over: the stream can stop, and later tool calls are not made.
func (t *ReplyTurn) Ended() bool { return t != nil && t.ended }

// Cut is the record of where the reply ended, nil while it has not.
func (t *ReplyTurn) Cut() *RunOnCut {
	if t == nil {
		return nil
	}
	return t.cut
}

func (t *ReplyTurn) take(s string) string {
	if s == "" {
		return ""
	}
	var out strings.Builder
	if t.boundary {
		t.boundary = false
		if !t.another("reasoning between messages") {
			t.cut.Drop(s)
			return ""
		}
		if !unicode.IsSpace(t.last) && !unicode.IsSpace(firstRune(s)) {
			t.add(&out, ' ')
		}
	}
	for i, r := range s {
		if t.glued(r) {
			if !t.another("glued to a sentence end") {
				t.cut.Drop(s[i:])
				return out.String()
			}
			t.add(&out, ' ')
		}
		t.add(&out, r)
	}
	return out.String()
}

func (t *ReplyTurn) add(out *strings.Builder, r rune) {
	out.WriteRune(r)
	t.kept.WriteRune(r)
	if r == '.' {
		t.lowerRunAtDot = t.lowerRun
	}
	if unicode.IsLower(r) {
		t.lowerRun++
	} else {
		t.lowerRun = 0
	}
	t.last = r
	if !unicode.IsSpace(r) {
		t.lastNonSpace = r
	}
	if r == '?' {
		t.asked = true
	}
}

// another decides whether the message starting now is still part of the reply.
func (t *ReplyTurn) another(why string) bool {
	if t.asked || t.extra >= 1 {
		t.ended = true
		reason := why + ", after a question"
		if !t.asked {
			reason = why + ", third message"
		}
		t.cut = TokenUsageFrom(t.ctx).NoteRunOn(reason, t.kept.String())
		return false
	}
	t.extra++
	return true
}

// glued reports whether r, written straight after the reply's last rune, starts a new message.
func (t *ReplyTurn) glued(r rune) bool {
	switch t.last {
	case '?', '!':
		// "?¿", "?Para", "?(", "?.", "?{" — not "?!", a closing quote or bracket, or ",;:".
		return !unicode.IsSpace(r) && !strings.ContainsRune(`?!"'»”’)],;:`, r)
	case '.', '…':
		// "tarde.¿Te", and "meant.What" after a lowercase word, which "EE.UU.", "p.m." and
		// "google.com" are not.
		return r == '¿' || r == '¡' || (unicode.IsUpper(r) && t.lowerRunAtDot >= 2)
	}
	return false
}

func isSentenceEnd(r rune) bool { return r == '?' || r == '!' || r == '.' || r == '…' }

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

// logRunOns leaves evidence of each reply cut where the model's next message began: what the caller
// heard is the kept part, and what the model went on to say is otherwise nowhere.
func (ms *ManagedStream) logRunOns(gen int, u *TokenUsage) {
	for _, c := range u.TakeRunOns() {
		ms.logger.Info("Reply cut: the model went on past its turn",
			"gen", gen, "reason", c.Reason(), "kept", clipForLog(c.Kept()), "dropped", clipForLog(c.Dropped()))
	}
}

func clipForLog(s string) string {
	if r := []rune(s); len(r) > 400 {
		return string(r[:400]) + "…"
	}
	return s
}
