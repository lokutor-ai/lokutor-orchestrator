package orchestrator

import (
	"context"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Whole-reply synthesis (2026-11-23, off unless TTS_WHOLE_REPLY=1): a reply's sentences are gathered and handed to the
// synthesiser as ONE text, so the voice model speaks the reply as one stream with one stop at its end. Today each
// sentence is its own request, and the voice model stops and restarts between them; on the production regression kit
// (652 replies, 1,440 sentences, Whisper large-v3) that restart is where its failures are: live L16c2v 8 capped, 14
// repeats, 49 sentences missing words per sentence against 5 / 0 / 6 as one text, and the LTH model 0 / 0 / 0 as one
// text (the TTS session's numbers). The price is first audio: nothing is synthesised until the reply's text is
// complete (or one of the two bounds below is reached).
//
// This only changes how a reply's text is grouped into synthesis requests. Nothing in the model's output is edited.
//
//   - TTS_WHOLE_REPLY_MAX_CHARS (0 = no limit): a chunk never grows past this many characters; the sentence that
//     would pass it starts the next chunk. The synthesiser's context holds the voice's reference window plus one
//     text and its audio, so a very long reply has to go in more than one piece.
//
//   - TTS_WHOLE_REPLY_WAIT_MS (0 = wait for the reply's end): the longest a chunk waits, counted from its first
//     sentence, before it is spoken with what has been gathered. Bounds what a slow model adds to first audio.
//
//   - TTS_WHOLE_REPLY_FIRST_WAIT_MS (unset = the same as TTS_WHOLE_REPLY_WAIT_MS): the bound for the reply's FIRST
//     chunk alone. Small (say 50), the reply is spoken in at most two pieces at no cost in first audio: the first is
//     whatever the model had written when the first sentence was ready (a short opener still waits for its sentence,
//     short_opener.go), and the second is everything written while the first was being synthesised, which is the rest
//     of the reply in practice: a sentence takes longer to synthesise than the model takes to finish. One restart
//     a reply, never after a short opener, instead of one per sentence.
//
// A chunk also ends where the reply does for the caller: at a tool call (the words before it are spoken while the
// tool runs) and when the model is asked again after an unspeakable sentence.
var (
	wholeReply          = strings.TrimSpace(os.Getenv("TTS_WHOLE_REPLY")) == "1"
	wholeReplyMaxChars  = envIntDefault("TTS_WHOLE_REPLY_MAX_CHARS", 0)
	wholeReplyWait      = time.Duration(envIntDefault("TTS_WHOLE_REPLY_WAIT_MS", 0)) * time.Millisecond
	wholeReplyFirstWait = time.Duration(envIntDefault("TTS_WHOLE_REPLY_FIRST_WAIT_MS", envIntDefault("TTS_WHOLE_REPLY_WAIT_MS", 0))) * time.Millisecond
)

// replyFlushMark in the sentence queue ends the chunk being gathered. It is never spoken.
const replyFlushMark = "\x00flush"

// gatherReply collects sentences from queue, starting with first, into one text. It returns when the queue closes, a
// replyFlushMark arrives, the next sentence would pass maxChars (returned as carry: it opens the next chunk), wait
// has passed since the call (wait > 0), or ctx ends. why names which.
func gatherReply(ctx context.Context, first string, queue <-chan string, maxChars int, wait time.Duration) (text, carry string, sentences int, why string) {
	text = strings.TrimSpace(first)
	sentences = 1
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	for {
		select {
		case next, ok := <-queue:
			if !ok {
				return text, "", sentences, "end"
			}
			if next == replyFlushMark {
				return text, "", sentences, "mark"
			}
			next = strings.TrimSpace(next)
			if next == "" {
				continue
			}
			if maxChars > 0 && utf8.RuneCountInString(text)+1+utf8.RuneCountInString(next) > maxChars {
				return text, next, sentences, "max_chars"
			}
			text += " " + next
			sentences++
		case <-timeout:
			// Whatever is already written goes with this chunk.
			for {
				select {
				case next, ok := <-queue:
					if !ok {
						return text, "", sentences, "end"
					}
					if next == replyFlushMark {
						return text, "", sentences, "mark"
					}
					if next = strings.TrimSpace(next); next == "" {
						continue
					}
					if maxChars > 0 && utf8.RuneCountInString(text)+1+utf8.RuneCountInString(next) > maxChars {
						return text, next, sentences, "max_chars"
					}
					text += " " + next
					sentences++
					continue
				default:
				}
				return text, "", sentences, "wait"
			}
		case <-ctx.Done():
			return text, "", sentences, "cancelled"
		}
	}
}

// groupWholeReply is the same grouping for a reply whose sentences are all known at once (a speculative hit, the
// reply after a tool): as few chunks as maxChars allows, cut only between sentences.
func groupWholeReply(segs []string, maxChars int) []string {
	var out []string
	cur := ""
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		switch {
		case cur == "":
			cur = s
		case maxChars > 0 && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(s) > maxChars:
			out = append(out, cur)
			cur = s
		default:
			cur += " " + s
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// speakWholeReply is the reply loop with whole-reply synthesis on: each chunk gathered from queue is one synthesis.
func (ms *ManagedStream) speakWholeReply(ctx, rctx context.Context, queue <-chan string, gen int) {
	carry := ""
	opening := true
	for {
		first := carry
		carry = ""
		if first == "" {
			t, ok := <-queue
			if !ok {
				return
			}
			if t == replyFlushMark || strings.TrimSpace(t) == "" {
				continue
			}
			first = t
		}
		began := time.Now()
		wait := wholeReplyWait
		if opening {
			wait = wholeReplyFirstWait
		}
		text, c, n, why := gatherReply(ctx, first, queue, wholeReplyMaxChars, wait)
		if opening && why == "wait" && isShortOpener(text) {
			// A lone "Vale." is never spoken alone while its sentence may still come (short_opener.go).
			if left := shortOpenerWait - time.Since(began); left > 0 {
				select {
				case next, ok := <-queue:
					switch {
					case !ok:
						why = "end"
					case next == replyFlushMark:
						why = "mark"
					case strings.TrimSpace(next) != "":
						text += " " + strings.TrimSpace(next)
						n++
						why = "opener"
					}
				case <-time.After(left):
				case <-ctx.Done():
					why = "cancelled"
				}
			}
		}
		opening = false
		carry = c
		ms.logger.Info("Whole reply gathered", "gen", gen, "sentences", n, "text_len", utf8.RuneCountInString(text),
			"waited_ms", time.Since(began).Milliseconds(), "ended_by", why)
		ms.speakText(rctx, text, gen)
		if why == "end" {
			return
		}
	}
}
