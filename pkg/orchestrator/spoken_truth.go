package orchestrator

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// Spoken truth: what the caller actually heard, and what they actually said.
//
// Synthesis runs faster than real time, so a reply is fully generated — and its text committed to the
// conversation and sent to every transcript as a BotResponse — long before the caller has heard it.
// When the caller cuts in two seconds into a six-second reply, the transcript and the model's own
// memory both said the agent delivered all six. Separately, a caller who paused mid-sentence and
// then carried on was split into two turns, and the second half was transcribed alone, without the
// first half as context, which is how "Pito ... Grillo" became two unrelated turns.
//
// The playout timeline below answers "what had the caller heard at time t": every audio chunk handed
// to the transport is placed on the caller's playout clock at max(now, end of the previous chunk),
// because both transports play in real time from a queue (the phone pacer sends 20 ms a tick; the
// browser plays as it receives) and both flush that queue the moment the caller starts speaking.

// interruptionMark ends a reply that was cut off, in the transcript and in the model's context, so
// neither reads a fragment as a finished sentence.
const interruptionMark = "…"

// continuationMergeMaxHeard is how much of a reply the caller may have heard and still have their
// next utterance treated as the continuation of the previous one. Below it the agent effectively
// had not started talking, so the two halves are one turn. Above it the caller heard real words and
// the interruption is a turn of its own. Overridable with CONTINUATION_MERGE_MAX_HEARD_MS.
func continuationMergeMaxHeard() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CONTINUATION_MERGE_MAX_HEARD_MS"))); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 1200 * time.Millisecond
}

// continuationMergeWindow bounds how far back a previous utterance may be and still be merged with the
// next one: a caller resuming a sentence does so within seconds.
const continuationMergeWindow = 8 * time.Second

// continuationGap is the silence placed between the two halves of a merged utterance, so the
// recogniser hears a pause rather than two words run together.
const continuationGap = 200 * time.Millisecond

type playoutInterval struct{ start, end time.Time }

// spokenSegment is one speakText call (one sentence, usually) on the caller's playout timeline.
type spokenSegment struct {
	id        int
	text      string
	intervals []playoutInterval
}

func (s *spokenSegment) total() time.Duration {
	var d time.Duration
	for _, iv := range s.intervals {
		d += iv.end.Sub(iv.start)
	}
	return d
}

func (s *spokenSegment) heardBefore(at time.Time) time.Duration {
	var d time.Duration
	for _, iv := range s.intervals {
		if !iv.start.Before(at) {
			break
		}
		end := iv.end
		if end.After(at) {
			end = at
		}
		d += end.Sub(iv.start)
	}
	return d
}

// playoutTimeline is one response generation's audio as the caller hears it.
type playoutTimeline struct {
	gen  int
	end  time.Time
	segs []spokenSegment
}

// schedule places dur of audio for segment (id, text) of generation gen on the timeline, as handed to
// the transport at now. A new generation starts a new timeline.
func (p *playoutTimeline) schedule(gen, id int, text string, now time.Time, dur time.Duration) {
	if gen != p.gen {
		*p = playoutTimeline{gen: gen}
	}
	start := p.end
	if start.Before(now) {
		start = now
	}
	p.end = start.Add(dur)
	n := len(p.segs)
	if n == 0 || p.segs[n-1].id != id {
		p.segs = append(p.segs, spokenSegment{id: id, text: text})
		n++
	}
	seg := &p.segs[n-1]
	if k := len(seg.intervals); k > 0 && seg.intervals[k-1].end.Equal(start) {
		seg.intervals[k-1].end = p.end
	} else {
		seg.intervals = append(seg.intervals, playoutInterval{start, p.end})
	}
}

// cutAt drops everything scheduled after at: the transport flushed its queue there (a tentative
// barge-in), so audio scheduled later was never played. If the barge-in turns out to be a false
// alarm, resumed audio is scheduled from the resume point.
func (p *playoutTimeline) cutAt(at time.Time) {
	if p.end.After(at) {
		p.end = at
	}
	for i := range p.segs {
		seg := &p.segs[i]
		kept := seg.intervals[:0]
		for _, iv := range seg.intervals {
			if !iv.start.Before(at) {
				continue
			}
			if iv.end.After(at) {
				iv.end = at
			}
			kept = append(kept, iv)
		}
		seg.intervals = kept
	}
}

// shiftFrom delays everything scheduled after at by d: the transport paused its queue at at and
// resumed it d later. An interval spanning at is split.
func (p *playoutTimeline) shiftFrom(at time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	if p.end.After(at) {
		p.end = p.end.Add(d)
	}
	for i := range p.segs {
		seg := &p.segs[i]
		var out []playoutInterval
		for _, iv := range seg.intervals {
			switch {
			case !iv.start.Before(at):
				out = append(out, playoutInterval{iv.start.Add(d), iv.end.Add(d)})
			case iv.end.After(at):
				out = append(out, playoutInterval{iv.start, at}, playoutInterval{at.Add(d), iv.end.Add(d)})
			default:
				out = append(out, iv)
			}
		}
		seg.intervals = out
	}
}

// heard is what the caller had heard of this generation at time at: every segment whose audio had
// fully played, then the words of the segment playing at that moment in proportion to how much of its
// audio had played (rounded down: a word half-heard is not claimed). heardDur is the audio played.
// complete reports whether every scheduled segment had fully played.
func (p *playoutTimeline) heard(at time.Time) (text string, heardDur time.Duration, complete bool) {
	var parts []string
	complete = true
	for i := range p.segs {
		seg := &p.segs[i]
		total := seg.total()
		h := seg.heardBefore(at)
		heardDur += h
		if total <= 0 {
			continue
		}
		// A few milliseconds short of the end is the end: chunk boundaries and clock reads are not exact.
		if h >= total-20*time.Millisecond {
			if t := strings.TrimSpace(seg.text); t != "" {
				parts = append(parts, t)
			}
			continue
		}
		complete = false
		if h > 0 {
			words := strings.Fields(seg.text)
			n := int(float64(len(words)) * float64(h) / float64(total))
			if n > 0 {
				parts = append(parts, strings.Join(words[:n], " "))
			}
		}
		break
	}
	return strings.Join(parts, " "), heardDur, complete
}

// withInterruptionMark ends a cut-off fragment with the interruption mark, replacing any trailing
// punctuation that would make it read as a finished sentence.
func withInterruptionMark(fragment string) string {
	f := strings.TrimSpace(fragment)
	if f == "" {
		return ""
	}
	f = strings.TrimRight(f, " ,;:.!?¡¿—-")
	if f == "" {
		return ""
	}
	return f + interruptionMark
}

// TruncatedResponse is the data of a BotResponseTruncated event: the caller cut the agent off, and
// this is what they actually heard. Transports replace the agent's transcript line for this reply
// (Full, if they received it as a BotResponse) with Spoken, or drop it when Spoken is empty.
type TruncatedResponse struct {
	// Full is the reply as generated. Empty when the reply was cut before it was complete.
	Full string `json:"full"`
	// Spoken is what the caller heard, ending with the interruption mark when cut mid-reply; empty
	// when nothing was heard (or the caller's next words are being merged into their previous turn).
	Spoken string `json:"spoken"`
	// HeardMs is how much of the reply's audio had played when the caller started speaking.
	HeardMs int64 `json:"heard_ms"`
	// Emitted reports whether a BotResponse carrying Full was emitted earlier for this reply.
	Emitted bool `json:"emitted"`
}

// RevisedTranscript is the data of a TranscriptRevised event: the caller's previous turn and their
// continuation were one utterance, transcribed together. Transports replace their last user line
// (Previous) with Text; it is emitted instead of a TranscriptFinal for the continuation.
type RevisedTranscript struct {
	Previous string `json:"previous"`
	Text     string `json:"text"`
}

// committedUtterance is a caller utterance kept so its continuation can be transcribed with it.
type committedUtterance struct {
	audio      []byte
	transcript string
	endedAt    time.Time
	// gen is ms.payloadGen when it was committed: its reply is any later generation.
	gen int
}

// joinUtteranceAudio is prev + a short silence + next, as one recogniser input (16-bit PCM).
func joinUtteranceAudio(prev, next []byte, sampleRate int) []byte {
	gap := int(continuationGap.Seconds()*float64(sampleRate)) * 2
	out := make([]byte, 0, len(prev)+gap+len(next))
	out = append(out, prev...)
	out = append(out, make([]byte, gap)...)
	out = append(out, next...)
	return out
}

// mergeableTail reports whether everything after the last user message in ctx is something a merge may
// discard or keep: assistant replies (discarded) and system notes (kept, e.g. retrieved knowledge). A
// tool call in between means the previous turn did real work, and it is not merged.
func mergeableTail(msgs []Message) (lastUser int, ok bool) {
	lastUser = -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return -1, false
	}
	for _, m := range msgs[lastUser+1:] {
		if m.Role != "assistant" && m.Role != "system" {
			return lastUser, false
		}
		if m.ToolCalls != nil {
			return lastUser, false
		}
	}
	return lastUser, true
}

// replaceLastUserTurn rewrites ctx so the last user message reads merged and no assistant reply
// follows it. It returns the rewritten slice and whether it applied.
func replaceLastUserTurn(msgs []Message, merged string) ([]Message, bool) {
	lastUser, ok := mergeableTail(msgs)
	if !ok {
		return msgs, false
	}
	out := make([]Message, 0, len(msgs))
	out = append(out, msgs[:lastUser]...)
	u := msgs[lastUser]
	u.Content = merged
	out = append(out, u)
	for _, m := range msgs[lastUser+1:] {
		if m.Role == "assistant" {
			continue
		}
		out = append(out, m)
	}
	return out, true
}

// segmentRef names the speakText call whose audio is currently going out.
type segmentRef struct {
	gen, id int
	text    string
}

// heardSnapshot is what the caller had heard of generation gen when they started speaking over it.
type heardSnapshot struct {
	gen int
	// seq is the utterance this onset starts (ms.utteranceSeq+1 at VAD start).
	seq      int
	at       time.Time
	text     string
	dur      time.Duration
	complete bool
}

// replyRecord is the text last sent to transports as a BotResponse, and for which generation.
type replyRecord struct {
	gen  int
	text string
}

// beginSegmentLocked is called by speakText (ms.mu held) before it synthesizes text for gen.
func (ms *ManagedStream) beginSegmentLocked(gen int, text string) {
	ms.segSeq++
	ms.curSeg = segmentRef{gen: gen, id: ms.segSeq, text: text}
}

// scheduleAudioLocked puts n bytes of outbound 16-bit audio for gen on the caller's playout clock
// (ms.mu held). Called only for audio actually handed to the transport.
func (ms *ManagedStream) scheduleAudioLocked(gen, n int, now time.Time) {
	if ms.playbackRate <= 0 || n <= 0 {
		return
	}
	dur := time.Duration(n) * time.Second / time.Duration(int64(ms.playbackRate)*2)
	id, text := -1, ""
	if ms.curSeg.gen == gen {
		id, text = ms.curSeg.id, ms.curSeg.text
	}
	ms.playout.schedule(gen, id, text, now, dur)
}

// snapshotHeardAtOnsetLocked records what the caller had heard of the current reply at now, the
// moment they started speaking over it (ms.mu held). The transport flushes its queue at this moment,
// so nothing scheduled later on the timeline was played.
func (ms *ManagedStream) snapshotHeardAtOnsetLocked(now time.Time) {
	gen := ms.payloadGen
	s := heardSnapshot{gen: gen, seq: ms.utteranceSeq + 1, at: now}
	if ms.playout.gen == gen {
		s.text, s.dur, s.complete = ms.playout.heard(now)
		if !ms.pausesOnBargeIn {
			// The transport discards its queue: nothing scheduled after now will ever play.
			ms.playout.cutAt(now)
		}
	}
	ms.onset = s
}

// SetTransportPausesOnBargeIn declares that the transport pauses its playback queue when the caller
// starts speaking (UserSpeaking), resumes it on BotResumed and discards it only on Interrupted. The
// phone transport does; a browser that stops playback outright does not. With it, speech over a
// reply that has finished synthesizing but is still playing is a tentative barge-in that a false
// alarm resumes, instead of a cut the caller can never get back.
func (ms *ManagedStream) SetTransportPausesOnBargeIn(v bool) {
	ms.mu.Lock()
	ms.pausesOnBargeIn = v
	ms.mu.Unlock()
}

// emitBotResponse sends a reply's text as a BotResponse, remembering it so an interruption can say
// which transcript line it corrects.
func (ms *ManagedStream) emitBotResponse(text string) {
	ms.mu.Lock()
	gen := ms.payloadGen
	ms.lastReply = replyRecord{gen: gen, text: text}
	ms.mu.Unlock()
	ms.emitWithGen(BotResponse, text, gen)
}

func (ms *ManagedStream) emitBotResponseWithGen(text string, gen int) {
	ms.mu.Lock()
	ms.lastReply = replyRecord{gen: gen, text: text}
	ms.mu.Unlock()
	ms.emitWithGen(BotResponse, text, gen)
}

// spokenTruthLocked is what the caller heard of the current reply, for an interruption confirmed now
// (ms.mu held). ok is false when there is no reply to speak of: nothing was sent or played.
func (ms *ManagedStream) spokenTruthLocked(now time.Time) (tr TruncatedResponse, ok bool) {
	gen := ms.payloadGen
	emitted := ms.lastReply.gen == gen
	played := ms.playout.gen == gen && len(ms.playout.segs) > 0
	if !emitted && !played {
		return tr, false
	}
	var text string
	var dur time.Duration
	complete := false
	switch {
	case ms.onset.gen == gen && !ms.onset.at.IsZero():
		text, dur, complete = ms.onset.text, ms.onset.dur, ms.onset.complete
	case played:
		text, dur, complete = ms.playout.heard(now)
	}
	tr = TruncatedResponse{HeardMs: dur.Milliseconds(), Emitted: emitted}
	if emitted {
		tr.Full = ms.lastReply.text
	}
	switch {
	case ms.mergeOnConfirm:
		tr.Spoken = ""
	case complete && emitted && sameWords(text, tr.Full):
		tr.Spoken = strings.TrimSpace(tr.Full)
	default:
		tr.Spoken = withInterruptionMark(text)
	}
	return tr, true
}

func sameWords(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// synthesizedTextLocked is the text of every segment of gen that reached the transport (ms.mu held):
// the provisional record of a reply whose pipeline was cancelled before it committed itself.
func (ms *ManagedStream) synthesizedTextLocked(gen int) string {
	if ms.playout.gen != gen {
		return ""
	}
	var parts []string
	for _, s := range ms.playout.segs {
		if t := strings.TrimSpace(s.text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// applySpokenTruthToContext makes the model's memory of the interrupted reply match what the caller
// heard: the reply to the last user turn is rewritten to tr.Spoken, removed when nothing was heard,
// or added when the reply was cut before it committed itself to context.
func (ms *ManagedStream) applySpokenTruthToContext(gen int, tr TruncatedResponse) {
	ms.replyCommitMu.Lock()
	defer ms.replyCommitMu.Unlock()
	ms.mu.Lock()
	ms.truthAppliedGen = gen
	ms.mu.Unlock()

	spoken := strings.TrimSpace(tr.Spoken)
	s := ms.session
	s.mu.Lock()
	lastUser := -1
	for i := len(s.Context) - 1; i >= 0; i-- {
		if s.Context[i].Role == "user" {
			lastUser = i
			break
		}
	}
	idx := -1
	for i := len(s.Context) - 1; i > lastUser; i-- {
		if s.Context[i].Role == "assistant" && s.Context[i].ToolCalls == nil {
			idx = i
			break
		}
	}
	switch {
	case idx >= 0 && spoken == "":
		s.Context = append(s.Context[:idx], s.Context[idx+1:]...)
		s.LastAssistant = ""
		s.mu.Unlock()
		ms.logger.Info("Spoken truth: removed a reply the caller did not hear", "gen", gen, "heard_ms", tr.HeardMs)
		return
	case idx >= 0:
		before := len(s.Context[idx].Content)
		s.Context[idx].Content = spoken
		s.LastAssistant = spoken
		s.mu.Unlock()
		ms.logger.Info("Spoken truth: reply truncated to what the caller heard",
			"gen", gen, "full_len", before, "spoken_len", len(spoken), "heard_ms", tr.HeardMs)
		return
	}
	s.mu.Unlock()
	if spoken != "" {
		s.AddMessage("assistant", spoken)
		ms.logger.Info("Spoken truth: recorded the heard part of a reply cut before it completed",
			"gen", gen, "spoken_len", len(spoken), "heard_ms", tr.HeardMs)
	}
}

// commitStreamedReply is the streaming path's commit of a finished (or cancelled) reply, ordered
// against applySpokenTruthToContext so the two never both write it.
func (ms *ManagedStream) commitStreamedReply(ctx context.Context, gen int, response, userTranscript string) {
	ms.replyCommitMu.Lock()
	defer ms.replyCommitMu.Unlock()
	if ctx.Err() == nil {
		if response != "" {
			ms.session.AddMessage("assistant", response)
			ms.emitBotResponseWithGen(response, gen)
			ms.cacheResponse(userTranscript, response, nil)
		}
		return
	}
	// Cancelled before the stream completed. If an interruption already wrote what was heard, that is
	// the record. Otherwise the pipeline was cancelled some other way (a new utterance ending, or a
	// barge-in not yet confirmed): record what reached the caller for now, and a confirmed
	// interruption rewrites it to what they actually heard.
	ms.mu.Lock()
	applied := ms.truthAppliedGen == gen
	provisional := ms.synthesizedTextLocked(gen)
	ms.mu.Unlock()
	if !applied && provisional != "" {
		ms.session.AddMessage("assistant", provisional)
	}
}

// reviseLastUserTurn replaces the caller's last turn in context with the merged transcript, dropping
// any reply that followed it. Reports whether it applied.
func (s *ConversationSession) reviseLastUserTurn(merged string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := replaceLastUserTurn(s.Context, merged)
	if ok {
		s.Context = out
		s.LastAssistant = ""
	}
	return ok
}

// continuationBase is the earlier utterance the one being processed continues, if any (ms.mu held).
//
//   - carry: the previous utterance looked unfinished, the turn waited, and the caller did resume —
//     its response was abandoned and it never reached the transcript or the model.
//   - barge: the caller started speaking over the reply to their previous utterance before hearing
//     more than continuationMergeMaxHeard of it; the reply is dropped and the two halves are one turn.
//
// isBarge reports the second case, which needs the transcript and context revised after the fact.
func (ms *ManagedStream) continuationBaseLocked(now time.Time, seq int, pendingBarge bool) (base *committedUtterance, isBarge bool) {
	if c := ms.takeCarryLocked(now); c != nil {
		return c, false
	}
	if b := ms.bargeBaseLocked(now, seq, pendingBarge); b != nil {
		return b, true
	}
	return nil, false
}

// takeCarryLocked consumes the fragment the mid-thought wait abandoned, if it is recent.
func (ms *ManagedStream) takeCarryLocked(now time.Time) *committedUtterance {
	c := ms.carry
	ms.carry = nil
	if c != nil && now.Sub(c.endedAt) <= continuationMergeWindow {
		return c
	}
	return nil
}

// bargeBaseLocked is the previous utterance, if the one being processed was spoken over its reply
// before the caller heard more than continuationMergeMaxHeard of it.
func (ms *ManagedStream) bargeBaseLocked(now time.Time, seq int, pendingBarge bool) *committedUtterance {
	// Over a reply: either a tentative barge-in on a reply still being generated, or the caller
	// starting to speak while a finished reply was still playing (this utterance's onset).
	gen := ms.payloadGen
	overReply := pendingBarge || (ms.onset.gen == gen && ms.onset.seq == seq && !ms.onset.at.IsZero())
	u := ms.lastUtt
	if !overReply || u == nil || now.Sub(u.endedAt) > continuationMergeWindow {
		return nil
	}
	// The reply being interrupted must be the answer to u: allocated after u was committed.
	if gen <= u.gen {
		return nil
	}
	if ms.onset.gen != gen || ms.onset.at.IsZero() || ms.onset.dur > continuationMergeMaxHeard() {
		return nil
	}
	return u
}

// acceptMerged reports whether a joint transcription of two halves is plausibly better than the
// continuation alone. A recogniser handed a noisy first half can return less than it did for the
// second half by itself ("Gracias." + "Gracias." came back as "Oh"); that is not a merge.
func acceptMerged(merged, continuation string) bool {
	// Both halves are in it, so it must say more than the second half alone.
	return merged != "" && countWords(merged) > countWords(continuation)
}

// transcribeJoined transcribes base and this utterance's audio as one.
func (ms *ManagedStream) transcribeJoined(ctx context.Context, base *committedUtterance, audio []byte, own string, overReply bool) (merged string, joined []byte, ok bool) {
	joined = joinUtteranceAudio(base.audio, audio, int(ms.inputSampleRate))
	res, err := ms.orch.Transcribe(ctx, joined, ms.session.GetCurrentLanguage())
	merged = strings.TrimSpace(res.Text)
	if err != nil || !acceptMerged(merged, own) {
		ms.logger.Info("Continuation: joint transcription not used, keeping the two halves separate",
			"previous", base.transcript, "continuation", own, "merged", merged, "error", err)
		return "", nil, false
	}
	merged = restoreQuestionMark(merged, ms.session.GetCurrentLanguage())
	ms.logger.Info("Continuation: transcribed together with the previous utterance",
		"previous", base.transcript, "continuation", own, "merged", merged,
		"over_reply", overReply, "joined_ms", len(joined)*1000/(int(ms.inputSampleRate)*2))
	return merged, joined, true
}

// noteSpeechOverPlayout handles the caller starting to speak while a reply whose synthesis has
// already finished is still playing. Synthesis runs faster than real time, so this is the common
// case: the stream is Idle (speakText has returned) while the caller hears the rest of the reply.
// Both transports stop playback the moment the caller speaks — the phone pacer flushes its queue on
// UserSpeaking and the browser is told "interrupted" — and nothing can resume it, so what the
// caller heard is settled now: record it, in context and in every transcript.
func (ms *ManagedStream) noteSpeechOverPlayout(now time.Time) {
	ms.mu.Lock()
	gen := ms.payloadGen
	if ms.playout.gen != gen || !ms.playout.end.After(now) || ms.truthAppliedGen == gen {
		ms.mu.Unlock()
		return
	}
	ms.snapshotHeardAtOnsetLocked(now)
	tr, ok := ms.spokenTruthLocked(now)
	ms.mu.Unlock()
	if !ok || (tr.Emitted && tr.Spoken == strings.TrimSpace(tr.Full)) {
		return
	}
	ms.applySpokenTruthToContext(gen, tr)
	ms.emitWithGen(BotResponseTruncated, tr, gen)
}
