package orchestrator

import (
	"context"
	"os"
	"regexp"
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
	// sched is all the audio ever scheduled for it. cutAt shortens intervals to what played; sched
	// keeps the whole length, so a sentence cut short can still be told from one that played out.
	sched time.Duration
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
	seg.sched += dur
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

// firstUnheard is the index of the first segment the caller had not heard to its end by at, or
// len(p.segs) when they had heard them all. Unlike heard, it holds after cutAt.
func (p *playoutTimeline) firstUnheard(at time.Time) int {
	for i := range p.segs {
		if p.segs[i].heardBefore(at) < p.segs[i].sched-20*time.Millisecond {
			return i
		}
	}
	return len(p.segs)
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
	// seq is its utterance number, the unit its turn-completion experiment row is filed under.
	seq int
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
	// stopped: a transport that plays through a barge-in has been told to stop (StopPlayback), and
	// text/dur/complete are what was heard at that moment rather than at the onset.
	stopped bool
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
	if ms.echoReplyGen != gen {
		ms.echoReplyGen, ms.echoReplyText = gen, ""
	}
	ms.echoReplyText = strings.TrimSpace(ms.echoReplyText + " " + text)
	if n := len(ms.echoReplyText); n > 2000 {
		cut := ms.echoReplyText[n-2000:]
		if i := strings.IndexByte(cut, ' '); i >= 0 {
			cut = cut[i+1:] // whole words only
		}
		ms.echoReplyText = cut
	}
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
// moment they started speaking over it (ms.mu held). A transport that stops outright flushes its
// queue at this moment, so nothing scheduled later on the timeline was played; one that pauses holds
// it, and one that plays through keeps playing until stopTransportPlayThrough or a confirmation.
func (ms *ManagedStream) snapshotHeardAtOnsetLocked(now time.Time) {
	gen := ms.payloadGen
	s := heardSnapshot{gen: gen, seq: ms.utteranceSeq + 1, at: now}
	if ms.playout.gen == gen {
		s.text, s.dur, s.complete = ms.playout.heard(now)
		if !ms.pausesOnBargeIn && !ms.playsThroughBargeIn {
			// The transport discards its queue: nothing scheduled after now will ever play.
			ms.playout.cutAt(now)
			ms.dropUnplayedFarEndEcho(now)
		}
	}
	ms.onset = s
}

// rereadHeardForPlayThroughLocked updates the onset snapshot to what the caller has heard by now, for
// a transport that kept playing through the barge-in and has not been stopped yet (ms.mu held).
func (ms *ManagedStream) rereadHeardForPlayThroughLocked(now time.Time) {
	gen := ms.payloadGen
	if !ms.playsThroughBargeIn || ms.onset.gen != gen || ms.onset.at.IsZero() || ms.onset.stopped {
		return
	}
	if ms.playout.gen == gen {
		ms.onset.text, ms.onset.dur, ms.onset.complete = ms.playout.heard(now)
	}
}

// playThroughStopAfter is how long a caller must keep talking over the agent before a transport that
// plays through tentative barge-ins is told to stop. Measured from VAD's own speech start, which
// already sits a confirmation window after the caller's first sound. Env BARGE_IN_PLAYTHROUGH_MS.
func playThroughStopAfter() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("BARGE_IN_PLAYTHROUGH_MS")); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 400 * time.Millisecond
}

// stopPlayThroughIfDue tells a play-through transport to stop once the caller has kept talking over
// the reply for playThroughStopAfter (StopPlayback). The transport discards what it holds, so what
// was heard is settled at this moment — as it is at the onset for a transport that stops outright,
// only later: a blip too short to reach this point never cost the caller any of the reply.
func (ms *ManagedStream) stopPlayThroughIfDue(now time.Time) {
	ms.mu.Lock()
	gen := ms.payloadGen
	if !ms.playsThroughBargeIn || !ms.pendingBargeIn || ms.pendingBargeGen != gen ||
		ms.onset.gen != gen || ms.onset.at.IsZero() || ms.onset.stopped ||
		now.Sub(ms.onset.at) < playThroughStopAfter() {
		ms.mu.Unlock()
		return
	}
	// Speech that is the agent's own voice coming back through the caller's microphone is not the
	// caller talking over it: keep playing. Stopping here cannot be undone — the transport discards
	// its queue — and the echo check after transcription used to run only once this had already
	// cut the reply. Checked on every frame until the barge-in resolves, so real speech mixed in
	// with the echo (which breaks the match) still stops playback.
	if echo, score, lagMs, _ := ms.isLikelyAcousticEcho(gen); echo {
		if !ms.echoHeldPlayThroughAt.Equal(ms.onset.at) {
			ms.echoHeldPlayThroughAt = ms.onset.at
			ms.logger.Info("Speech over playout matches the agent's own audio: playing through",
				"score", score, "lag_ms", lagMs, "talked_ms", now.Sub(ms.onset.at).Milliseconds())
		}
		ms.mu.Unlock()
		return
	}
	talkedMs := now.Sub(ms.onset.at).Milliseconds()
	ms.onset.stopped = true
	var tr TruncatedResponse
	settle := false
	if ms.playout.gen == gen {
		ms.onset.text, ms.onset.dur, ms.onset.complete = ms.playout.heard(now)
		ms.playout.cutAt(now)
		ms.dropUnplayedFarEndEcho(now)
		// With no turn in flight the reply was over and only playing out: settle what was heard now,
		// as noteSpeechOverPlayout does. A turn still in flight settles on its own interrupt.
		if ms.truthAppliedGen != gen && (ms.pipelineCtx == nil || ms.pipelineCtx.Err() != nil) {
			tr, settle = ms.spokenTruthLocked(now)
		}
	}
	ms.mu.Unlock()
	ms.logger.Info("Caller kept talking over the agent: stopping playback", "talked_ms", talkedMs)
	ms.emitWithGen(StopPlayback, nil, gen)
	if settle && !(tr.Emitted && tr.Spoken == strings.TrimSpace(tr.Full)) {
		ms.applySpokenTruthToContext(gen, tr)
		ms.emitWithGen(BotResponseTruncated, tr, gen)
	}
}

// SetTransportPlaysThroughBargeIn declares that the transport keeps playing when the caller starts
// speaking over the agent and stops only when told: StopPlayback once they have kept talking for
// BARGE_IN_PLAYTHROUGH_MS (400 ms), or Interrupted when the barge-in is confirmed. A false alarm
// (BotResumed) never interrupted anything. For a transport that can only discard its queue — a
// browser — this is what keeps a cough, a click or the agent's own echo from cutting a reply for
// good: every speech onset used to stop playback outright, and a 217 ms blip 1 s into a greeting,
// transcribed as nothing, cut the greeting at 13 of 175 characters (2026-09-23). Openings suffer most,
// because the browser's echo canceller has not adapted yet in the first second of a call.
func (ms *ManagedStream) SetTransportPlaysThroughBargeIn(v bool) {
	ms.mu.Lock()
	ms.playsThroughBargeIn = v
	ms.mu.Unlock()
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

// replyUnheardLocked reports whether gen's reply was abandoned before any of it reached the caller:
// speakText's resume check discarded a sentence of it and no audio of it was ever sent (ms.mu held).
func (ms *ManagedStream) replyUnheardLocked(gen int) bool {
	return gen != 0 && ms.resumeDiscardGen == gen && ms.synthesizedTextLocked(gen) == ""
}

// withdrawUnheardReply takes back a reply that was put in context and the transcript before it was
// spoken, when none of it was. A speculative hit and a non-streaming model have the whole reply in
// hand and record it first; speakText's resume check can then abandon every sentence of it. The
// streaming path commits after speaking and simply does not record it (commitStreamedReply).
func (ms *ManagedStream) withdrawUnheardReply(gen int, response string) {
	ms.mu.Lock()
	unheard := ms.replyUnheardLocked(gen) && ms.truthAppliedGen != gen
	ms.mu.Unlock()
	if !unheard || strings.TrimSpace(response) == "" {
		return
	}
	// Spoken "" with Emitted: the reply leaves context (applySpokenTruthToContext logs it) and every
	// transcript drops its line, exactly as for a reply interrupted before a word of it was heard.
	tr := TruncatedResponse{Full: response, Emitted: true}
	ms.applySpokenTruthToContext(gen, tr)
	ms.emitWithGen(BotResponseTruncated, tr, gen)
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
		ms.mu.Lock()
		unheard := ms.replyUnheardLocked(gen)
		ms.mu.Unlock()
		if unheard && response != "" {
			// Nothing cancels the turn when speakText abandons a reply because the caller resumed, so
			// this is still the live-context branch — and it used to record the whole reply as said and
			// send it to the transcript. On 2026-09-28 (agent_1790600579818180595) the answer to "¿Puedes
			// reservarme la cita?" was discarded that way, both sentences, and still became the agent's
			// line in context and on screen; the caller never heard a word of it.
			ms.logger.Info("Reply not recorded: the caller resumed before any of it played",
				"gen", gen, "text_len", len(response))
			return
		}
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
	// Both halves are in it, so it must say more than the second half alone. Words are counted with
	// sentences split apart: the recogniser writes "Hola.¿Qué hora abre la oficina mañana?" with no
	// space, which counted as the continuation's six words and threw the merge away.
	return merged != "" && countWords(spaceSentences(merged)) > countWords(continuation)
}

var sentenceRunTogether = regexp.MustCompile(`([.?!])([¿¡\p{L}])`)

// spaceSentences puts a space between sentences the recogniser wrote without one ("Hola.¿Qué").
func spaceSentences(s string) string {
	return sentenceRunTogether.ReplaceAllString(s, "$1 $2")
}

// jointWaitBudget is the longest a turn waits for the joint transcription of a carried fragment and its
// continuation once the continuation's own transcript is ready. The joint pass used to run after that
// transcript, synchronously, as a second recognition call: under a burst of callers it was the largest
// term in time to first audio (1.7 s of a 4.1 s turn at 40 simultaneous callers, 2026-09-29, and its
// result then thrown away), and every such call added to the queue every other caller's first pass
// waits in. It now starts when the continuation arrives, beside that transcription, and a turn that
// would wait longer than this joins the two transcripts as text instead.
const jointWaitBudget = 150 * time.Millisecond

// jointTranscription is the joint pass over an earlier utterance -- a carried fragment, or the one whose
// reply the caller spoke over -- and the utterance that continues it.
type jointTranscription struct {
	base   *committedUtterance
	joined []byte
	cancel context.CancelFunc
	done   chan struct{}
	text   string
	err    error
}

// result is the joint pass's text and error once it has finished, and nothing while it is still
// running: when awaitJoint gives up on it (past jointWaitBudget) the goroutine may still be writing
// them, and the log lines that reported them read them anyway -- a data race, and the reason
// go test -race failed TestMergeBarge and TestMergeCarry.
func (j *jointTranscription) result() (string, error) {
	select {
	case <-j.done:
		return j.text, j.err
	default:
		return "", nil
	}
}

// needsJointPass reports whether a carried fragment gains anything from being transcribed again together
// with its continuation. A short fragment the recogniser closed as a sentence of its own ("Hola.",
// "Vale.", "Um.") does not: its words are settled and joining the text is exact, so the second
// recognition call is skipped. Anything longer or left open may be a phrase cut mid-thought whose words
// the continuation disambiguates ("Grillo" without "Pito").
func needsJointPass(fragment string) bool {
	f := strings.TrimSpace(fragment)
	if f == "" {
		return false
	}
	last := f[len(f)-1]
	return !(countWords(f) <= 2 && (last == '.' || last == '?' || last == '!'))
}

// startJointTranscription starts the joint pass now, beside the continuation's own transcription.
func (ms *ManagedStream) startJointTranscription(ctx context.Context, base *committedUtterance, audio []byte) *jointTranscription {
	jctx, cancel := context.WithCancel(ctx)
	j := &jointTranscription{
		base:   base,
		joined: joinUtteranceAudio(base.audio, audio, int(ms.inputSampleRate)),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	lang := ms.session.GetCurrentLanguage()
	go func() {
		defer close(j.done)
		res, err := ms.orch.Transcribe(jctx, j.joined, lang)
		j.text, j.err = strings.TrimSpace(res.Text), err
	}()
	return j
}

// mergeCarry makes one turn of a carried fragment and the utterance that continues it: the joint
// transcription when it is ready within jointWaitBudget and accepted, otherwise the two transcripts
// joined as text. The fragment is never dropped: when the joint pass was rejected the turn used to be
// the continuation alone, so "I'd like to book a flight to" + "Madrid on Friday" reached the model as
// "Madrid on Friday".
func (ms *ManagedStream) mergeCarry(ctx context.Context, base *committedUtterance, audio []byte, own string, joint *jointTranscription) (string, []byte) {
	asText := spaceSentences(strings.TrimSpace(base.transcript + " " + own))
	if joint == nil || joint.base != base {
		if joint != nil {
			joint.cancel()
		}
		reason := "fragment complete on its own"
		if needsJointPass(base.transcript) {
			reason = "joint pass not started"
		}
		ms.logger.Info("Continuation: joined as text", "previous", ContentForLog(base.transcript), "continuation", ContentForLog(own),
			"merged", ContentForLog(asText), "reason", reason)
		return asText, joinUtteranceAudio(base.audio, audio, int(ms.inputSampleRate))
	}
	merged, why := ms.awaitJoint(ctx, joint, own)
	if merged == "" {
		jointText, jointErr := joint.result()
		ms.logger.Info("Continuation: joint transcription "+why+", joined as text", "previous", ContentForLog(base.transcript),
			"continuation", ContentForLog(own), "joint", ContentForLog(jointText), "merged", ContentForLog(asText), "error", jointErr)
		return asText, joint.joined
	}
	ms.logger.Info("Continuation: transcribed together with the previous utterance",
		"previous", ContentForLog(base.transcript), "continuation", ContentForLog(own), "merged", ContentForLog(merged),
		"over_reply", false, "joined_ms", len(joint.joined)*1000/(int(ms.inputSampleRate)*2))
	return merged, joint.joined
}

// awaitJoint waits at most jointWaitBudget for a joint pass and returns its transcript if it is ready and
// accepted, or "" and why not. The wait is this turn's gate_join_ms.
func (ms *ManagedStream) awaitJoint(ctx context.Context, joint *jointTranscription, own string) (merged, why string) {
	waitStart := time.Now()
	defer func() {
		ms.mu.Lock()
		ms.joinWaitMs = time.Since(waitStart).Milliseconds()
		ms.mu.Unlock()
	}()
	select {
	case <-joint.done:
	case <-time.After(jointWaitBudget):
		joint.cancel()
		return "", "not ready"
	case <-ctx.Done():
		joint.cancel()
		return "", "cancelled"
	}
	if joint.err != nil || !acceptMerged(joint.text, own) {
		return "", "not used"
	}
	return restoreQuestionMark(spaceSentences(joint.text), ms.session.GetCurrentLanguage()), ""
}

// mergeBarge makes one turn of an utterance spoken over the reply to the previous one, when the caller
// heard next to nothing of that reply. ok=false keeps the two turns separate: the previous one is
// already in context as the caller's turn, so unlike a carried fragment nothing is lost.
//
// The joint pass used to start here, after the echo and barge-in checks, and wait for its answer: 1.9 s
// of a 3.5 s turn at 48 simultaneous callers (2026-09-29), to re-transcribe "Hola." and fail. Now:
//   - a previous utterance that settled on its own ("Hola.") is joined as text, which is what an
//     accepted joint pass produced for it, with no second recognition call;
//   - anything else uses the joint pass started beside this utterance's own transcription
//     (processUtterance), waiting at most jointWaitBudget for it.
func (ms *ManagedStream) mergeBarge(ctx context.Context, base *committedUtterance, audio []byte, own string, joint *jointTranscription) (string, []byte, bool) {
	if !needsJointPass(base.transcript) {
		merged := spaceSentences(strings.TrimSpace(base.transcript + " " + own))
		ms.logger.Info("Continuation: joined as text", "previous", ContentForLog(base.transcript), "continuation", ContentForLog(own),
			"merged", ContentForLog(merged), "over_reply", true, "reason", "previous utterance complete on its own")
		return merged, joinUtteranceAudio(base.audio, audio, int(ms.inputSampleRate)), true
	}
	if joint == nil || joint.base != base {
		ms.logger.Info("Continuation: joint pass not started, keeping the two halves separate",
			"previous", ContentForLog(base.transcript), "continuation", ContentForLog(own))
		return "", nil, false
	}
	merged, why := ms.awaitJoint(ctx, joint, own)
	if merged == "" {
		jointText, jointErr := joint.result()
		ms.logger.Info("Continuation: joint transcription "+why+", keeping the two halves separate",
			"previous", ContentForLog(base.transcript), "continuation", ContentForLog(own), "joint", ContentForLog(jointText), "error", jointErr)
		return "", nil, false
	}
	ms.logger.Info("Continuation: transcribed together with the previous utterance",
		"previous", ContentForLog(base.transcript), "continuation", ContentForLog(own), "merged", ContentForLog(merged),
		"over_reply", true, "joined_ms", len(joint.joined)*1000/(int(ms.inputSampleRate)*2))
	return merged, joint.joined, true
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

// replyResumableLocked reports whether settling the pending barge-in as a false alarm puts any of the
// reply back in the caller's ears, the way resolvePendingBargeInFor resumes it (ms.mu held): a turn
// still generating it, synthesis still running with frames held for it, or a transport still holding
// audio it has not played (a paused queue, or a play-through never told to stop).
func (ms *ManagedStream) replyResumableLocked(now time.Time) bool {
	gen := ms.payloadGen
	if ms.pipelineCancel != nil && ms.pipelineCtx != nil && ms.pipelineCtx.Err() == nil {
		return true
	}
	if ms.ttsCancel != nil && ms.heldAudioGen == gen && len(ms.heldAudio) > 0 {
		return true
	}
	if ms.onset.gen != gen || ms.onset.at.IsZero() {
		return false
	}
	switch {
	case ms.pausesOnBargeIn:
		return !ms.onset.complete
	case ms.playsThroughBargeIn:
		return !ms.onset.stopped && ms.playout.gen == gen && ms.playout.end.After(now)
	}
	return false
}

// lostReplyLocked is what the caller will not hear of gen's reply unless it is said again, once nothing
// resumes it (ms.mu held): from the start of the first sentence they had not heard to the end — a
// sentence cut short is said again whole — including sentences that never reached the transport (held
// during the barge-in and dropped, discarded by speakText's resume check, or never synthesised; the
// reply sent to the transcript, or else what began synthesis, says what they were). "" when they heard
// it all.
func (ms *ManagedStream) lostReplyLocked(gen int, now time.Time) string {
	var sent []string
	first := 0
	if ms.playout.gen == gen {
		first = ms.playout.firstUnheard(now)
		for _, s := range ms.playout.segs {
			sent = append(sent, strings.TrimSpace(s.text))
		}
	}
	lost := strings.Fields(strings.Join(sent[first:], " "))
	sentWords := strings.Fields(strings.Join(sent, " "))
	whole := ""
	switch {
	case ms.lastReply.gen == gen:
		whole = ms.lastReply.text
	case ms.echoReplyGen == gen:
		whole = ms.echoReplyText
	}
	if w := strings.Fields(whole); len(w) > len(sentWords) &&
		sameWords(strings.Join(w[:len(sentWords)], " "), strings.Join(sentWords, " ")) {
		lost = append(lost, w[len(sentWords):]...)
	}
	return strings.Join(lost, " ")
}

// settleEchoVeto decides what becomes of an utterance the echo check judged to be the agent's own voice,
// and reports whether it is to be answered after all. The veto is there to keep the reply playing: it
// drops the utterance and resumes the reply. When nothing of the reply was left to resume (a
// play-through transport already told to stop, the turn cancelled when the utterance ended) it dropped
// both, and the agent said nothing more until the caller spoke again or the silence check-in came 10 s
// later: 27 of the 89 vetoes in the 14 days to 2026-10-19 came after playback had been stopped, among
// them agent_1792403239900718816's, whose "What should I take?" was never answered.
//
//   - Something resumes: the reply plays on and the utterance is dropped, as before.
//   - The caller had heard the whole reply: nothing was lost, and the utterance is dropped.
//   - They had heard next to none of it (bargeBaseLocked) and only the acoustic check says echo: the
//     utterance is answered as the continuation of their previous one, the two joined as one turn. A
//     continuation is never dropped over a reply the caller did not hear; if it was the agent's voice
//     after all, the turn carries a few of its words.
//   - Otherwise the rest of the reply is said again, from the start of the sentence that was cut
//     (resayLostReply), unless the caller is already speaking again.
func (ms *ManagedStream) settleEchoVeto(ctx context.Context, seq, gen int, pendingBarge, carried bool, method string,
	acoustic acousticEchoReading, textEcho bool, transcript, botWasSaying string) (answer bool) {
	now := time.Now()
	ms.mu.Lock()
	resumable := ms.replyResumableLocked(now)
	lost := ""
	if !resumable {
		lost = ms.lostReplyLocked(gen, now)
	}
	joinable := !resumable && lost != "" && !textEcho && !carried && ms.bargeBaseLocked(now, seq, pendingBarge) != nil
	superseded := ms.utteranceSeq != seq || ms.vadSpeaking
	ms.mu.Unlock()

	outcome := "said_again"
	switch {
	case resumable:
		outcome = "resumed"
	case lost == "":
		outcome = "nothing_lost"
	case joinable:
		outcome = "joined_to_previous"
	case superseded:
		outcome = "superseded"
	}
	ms.logger.Info("Barge-in looks like an echo of the bot's own speech",
		"outcome", outcome, "method", method, "score", acoustic.score, "lag_ms", acoustic.lagMs,
		"lost_len", len(lost), "transcript", ContentForLog(transcript), "bot_was_saying", ContentForLog(botWasSaying))
	if outcome == "joined_to_previous" {
		return true
	}
	ms.resolvePendingBargeInFor(true)
	if outcome == "said_again" {
		ms.resayLostReply(ctx, gen, lost)
	}
	return false
}

// resayLostReply says again the part of gen's reply an echo cost the caller (settleEchoVeto). What they
// heard of it is settled first, as for an interruption (the context keeps the fragment, transcripts show
// it cut), and the rest goes out as a reply of its own: a new generation, its BotResponse, and an
// assistant message after the fragment, so an interruption of it is settled like any other. No model
// call: these are the words the model already gave.
func (ms *ManagedStream) resayLostReply(ctx context.Context, gen int, text string) {
	ms.mu.Lock()
	var tr TruncatedResponse
	settle := false
	if ms.payloadGen == gen && ms.truthAppliedGen != gen {
		tr, settle = ms.spokenTruthLocked(time.Now())
	}
	ms.mu.Unlock()
	if settle && !(tr.Emitted && tr.Spoken == strings.TrimSpace(tr.Full)) {
		ms.applySpokenTruthToContext(gen, tr)
		ms.emitWithGen(BotResponseTruncated, tr, gen)
	}

	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()
	ms.mu.Lock()
	if ms.payloadGen != gen {
		// A newer reply started in the meantime: that is the caller's answer now.
		ms.mu.Unlock()
		ms.logger.Info("Not saying the lost reply again: a newer reply started", "gen", gen)
		return
	}
	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
	}
	ms.pipelineCancel = rCancel
	ms.pipelineCtx = rCtx
	ms.payloadGen++
	newGen := ms.payloadGen
	ms.responseTrigger = "resay"
	ms.turnTokens = nil
	ms.discardStart = time.Time{}
	ms.mu.Unlock()

	now := time.Now()
	ms.llmStartTime, ms.llmEndTime = now, now // nothing to generate
	ms.ttsStartTime, ms.ttsFirstChunkTime = time.Time{}, time.Time{}
	ms.turnAudioBytes = 0
	ms.lastResponseText = text
	ms.spokenTextPrefix = ""
	ms.spokenTextLocked = false
	ms.responseChunksSent = 0
	ms.session.AddMessage("assistant", text)
	ms.emitBotResponse(text)
	ms.speakResponse(rCtx, text, newGen)
	ms.withdrawUnheardReply(newGen, text)
}
