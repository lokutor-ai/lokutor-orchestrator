package orchestrator

import (
	"math/rand"
	"time"
)

// The dead-air guard (2026-11-27, Config.DeadAirGuardMs). A person who needs a moment to answer makes a sound ("ah",
// "mm") within about a second; a silence past ~700 ms already reads as reluctance (Kendrick and Torreira 2015). When the
// reply's first audio is not out DeadAirGuardMs after the end-of-turn gate committed the caller's turn, the agent makes
// one short sound from the call's own bank (backchannel_sounds.go: cut from its own synthesis, in its voice and
// language) and the reply follows it: the sound goes out first and the reply's audio is scheduled after it
// (backchannelEnd), exactly as after a backchannel.
//
// It adds no words: the reply, the conversation and every transcript are untouched. Which sound: the category the
// agent's own model last picked for the caller's words (backchannel.go) when it fits a speaker about to answer (ack,
// sympathy, surprise), else ack ("ah"). Production, 2026-11-19..26 (finance/measurements/naturalness_scorecard_
// 2026-11-26): the first audio came more than 1 s after the commit on 13.6% of turns (web 10%, phone 31%), the LLM's
// first token the main part in 44 of 55; those are the turns it plays on at 1000 ms.
//
// Not when: a newer utterance exists, the caller is talking or a barge-in is pending, any audio of the reply (or a
// tool's spoken filler) is already out, a listener sound played in the last deadAirAfterBackchannel, the call is
// listening to a dictation, or the bank is not there.

const deadAirAfterBackchannel = 1500 * time.Millisecond

// armDeadAirGuard starts the guard for utterance seq, committed now.
func (ms *ManagedStream) armDeadAirGuard(seq int) {
	if ms.orch == nil || ms.orch.config.DeadAirGuardMs <= 0 || ms.clientVAD {
		return
	}
	ms.ensureBackchannelBank()
	committed := time.Now()
	after := time.Duration(ms.orch.config.DeadAirGuardMs) * time.Millisecond
	time.AfterFunc(after, func() { ms.deadAirGuardFire(seq, committed) })
}

// deadAirGuardFire plays the sound if the turn committed at committed still has no audio.
func (ms *ManagedStream) deadAirGuardFire(seq int, committed time.Time) {
	if ms.isClosed.Load() || ms.ctx.Err() != nil {
		return
	}
	now := time.Now()
	ms.mu.Lock()
	why := ""
	switch {
	case ms.utteranceSeq != seq:
		why = "newer_utterance"
	case ms.vadSpeaking || ms.pendingBargeIn:
		why = "caller_talking"
	case !ms.ttsFirstChunkTime.IsZero() && !ms.ttsFirstChunkTime.Before(committed):
		why = "" // the reply is out: nothing to do, nothing to log
		ms.mu.Unlock()
		return
	case ms.playout.end.After(now):
		why = "audio_playing"
	case ms.state != StateProcessing && ms.state != StateSpeaking:
		why = "not_answering"
	}
	ms.mu.Unlock()
	if why == "" && ms.listeningTo() != nil {
		why = "listening"
	}
	ms.bc.mu.Lock()
	pick := ms.bc.pick
	if why == "" && !ms.bc.lastAt.IsZero() && now.Sub(ms.bc.lastAt) < deadAirAfterBackchannel {
		why = "listener_sound_just_played"
	}
	ms.bc.mu.Unlock()
	waited := now.Sub(committed).Milliseconds()
	if why != "" {
		ms.logger.Info("Dead-air guard: no sound", "reason", why, "since_commit_ms", waited)
		return
	}
	bank := ms.ensureBackchannelBank()
	if bank == nil {
		ms.logger.Info("Dead-air guard: no sound", "reason", "no_sounds_yet", "since_commit_ms", waited)
		return
	}
	cat := BackchannelAck
	switch pick {
	case BackchannelAck, BackchannelSympathy, BackchannelSurprise:
		cat = pick
	}
	clips := bank.Clips[cat]
	if len(clips) == 0 {
		cat, clips = BackchannelNeutral, bank.Clips[BackchannelNeutral]
	}
	if len(clips) == 0 {
		ms.logger.Info("Dead-air guard: no sound", "reason", "no_sound_for_category", "since_commit_ms", waited)
		return
	}
	ms.bc.mu.Lock()
	clip := clips[rand.Intn(len(clips))]
	if len(clips) > 1 && sameBytes(clip, ms.bc.lastClip) {
		for _, c := range clips {
			if !sameBytes(c, ms.bc.lastClip) {
				clip = c
				break
			}
		}
	}
	ms.bc.lastClip, ms.bc.lastAt = clip, now
	ms.bc.mu.Unlock()
	if !ms.emitBackchannelAudio(clip) {
		return
	}
	ms.logger.Info("Dead-air guard: sound played", "category", cat, "pick", pick, "since_commit_ms", waited,
		"audio_ms", ms.pcmDuration(clip).Milliseconds())
}
