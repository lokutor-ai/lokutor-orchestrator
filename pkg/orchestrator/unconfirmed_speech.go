package orchestrator

import "time"

// Caller audio at speech level that the VAD never confirms as speech is never transcribed: the caller's words vanish
// with no utterance, no turn and, until this, no log line. On 2026-10-21 (agent_1792609348422485695, web) a caller's
// "How does Lokutor compare to ElevenLabs?" reached the VAD whole, at full level, but it scored everything after
// "How does Lok-" under its threshold (lastProbability 0.08, 0.02): the agent answered "How does Lock?" and the rest
// of the sentence left nothing behind. This keeps that evidence. It changes no decision.
//
// "Speech level" is relative to the caller's own voice: within unconfirmedBelowPeakDb of the loudest 20 ms of the
// speech the VAD last confirmed, the line the speculative transcript uses for a soft last word (specLoudBelowPeakDb).
// Before the caller's first confirmed speech there is nothing to compare with, so nothing is counted. An echo gate's
// zeroed frames are digital silence and never count.
//
// A burst is reported when it ends without the VAD confirming it, or once it has gone on unconfirmed for
// unconfirmedForceMs. One that ends in a confirmed onset is not: the 300 ms lead-in carries its start into the
// utterance.
const (
	unconfirmedBelowPeakDb = specLoudBelowPeakDb
	// unconfirmedLogMs of speech-level audio in a burst makes it reportable: longer than the VAD's own onset run
	// (192 ms in production).
	unconfirmedLogMs = 300
	// unconfirmedGapMs of quiet ends a burst.
	unconfirmedGapMs = 500
	// unconfirmedForceMs of speech-level audio is reported while the burst goes on.
	unconfirmedForceMs = 2000
)

// unconfirmedSpeech is touched only from handleAudio (the audio goroutine).
type unconfirmedSpeech struct {
	peakDb  float64 // loudest 20 ms of the last confirmed speech run; 0 = none yet
	runPeak float64 // loudest 20 ms of the run in progress; 0 = not in a run

	loudMs, quietMs int
	burstPeakDb     float64
	maxProb         float64
	endedAt         time.Time // when the VAD last stopped reporting speech
	reported        bool      // once per gap between confirmed speech runs
}

// unconfirmedReport is what one gap's evidence line says.
type unconfirmedReport struct {
	loudMs           int
	levelDb, belowDb float64
	maxProb          float64
	sinceSpeechMs    int64
}

// note takes one chunk at 16 kHz: speaking is the VAD's verdict on it, prob its latest probability (-1 when the VAD
// gives none). It returns a report when a gap's unconfirmed speech-level audio is due one.
func (u *unconfirmedSpeech) note(pcm16k []byte, speaking bool, prob float64, now time.Time) (unconfirmedReport, bool) {
	if len(pcm16k) < 2 {
		return unconfirmedReport{}, false
	}
	level := rmsDbfs(pcm16k)
	if speaking {
		if u.runPeak == 0 || level > u.runPeak {
			u.runPeak = level
		}
		u.resetBurst()
		u.reported = false
		return unconfirmedReport{}, false
	}
	if u.runPeak != 0 {
		u.peakDb, u.runPeak, u.endedAt = u.runPeak, 0, now
	}
	if u.peakDb == 0 || u.reported {
		return unconfirmedReport{}, false
	}
	ms := len(pcm16k) / 32
	if level < u.peakDb-unconfirmedBelowPeakDb {
		u.quietMs += ms
		if u.quietMs < unconfirmedGapMs {
			return unconfirmedReport{}, false
		}
		r, due := u.report(now, u.loudMs >= unconfirmedLogMs)
		u.resetBurst()
		return r, due
	}
	u.quietMs = 0
	u.loudMs += ms
	if u.burstPeakDb == 0 || level > u.burstPeakDb {
		u.burstPeakDb = level
	}
	if prob > u.maxProb {
		u.maxProb = prob
	}
	return u.report(now, u.loudMs >= unconfirmedForceMs)
}

func (u *unconfirmedSpeech) report(now time.Time, due bool) (unconfirmedReport, bool) {
	if !due {
		return unconfirmedReport{}, false
	}
	u.reported = true
	return unconfirmedReport{loudMs: u.loudMs, levelDb: u.burstPeakDb, belowDb: u.peakDb - u.burstPeakDb,
		maxProb: u.maxProb, sinceSpeechMs: now.Sub(u.endedAt).Milliseconds()}, true
}

func (u *unconfirmedSpeech) resetBurst() {
	u.loudMs, u.quietMs, u.burstPeakDb, u.maxProb = 0, 0, 0, 0
}

// noteUnconfirmedSpeech feeds the tracker from handleAudio and logs its report.
func (ms *ManagedStream) noteUnconfirmedSpeech(pcm16k []byte, speaking bool, now time.Time) {
	prob, thr := -1.0, -1.0
	if p, ok := ms.vad.(interface{ LastProbability() float64 }); ok {
		prob = p.LastProbability()
	}
	r, ok := ms.unconfirmed.note(pcm16k, speaking, prob, now)
	if !ok {
		return
	}
	if p, ok := ms.vad.(interface{ LastEffectiveThreshold() float64 }); ok {
		thr = p.LastEffectiveThreshold()
	}
	ms.mu.Lock()
	agentPlaying := ms.playout.gen == ms.payloadGen && ms.playout.end.After(now)
	state := ms.state
	ms.mu.Unlock()
	ms.logger.Info("Caller audio at speech level that the VAD did not confirm as speech: not transcribed",
		"loud_ms", r.loudMs, "level_dbfs", r.levelDb, "below_speech_peak_db", r.belowDb,
		"max_probability", r.maxProb, "threshold", thr, "since_speech_ms", r.sinceSpeechMs,
		"agent_playing", agentPlaying, "state", state)
}
