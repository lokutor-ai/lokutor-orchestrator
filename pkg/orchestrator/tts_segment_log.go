package orchestrator

import "time"

// Per-segment timing of an agent turn's audio (2026-11-04, for the TTS session's naturalness work). A reply is spoken one segment per
// speakText call; the pause a caller hears between two segments is the previous segment's trailing silence, the next one's leading
// silence, and any underrun (the next segment's first audio was not ready when the previous one's audio ended). One "tts_segment" line per
// segment carries what is needed to tell them apart. No text: only lengths and times.

// segmentSilenceRMS is the PCM16 RMS under which a 10 ms window counts as silence (about -45 dBFS).
const segmentSilenceRMS = 184

type segmentProbe struct {
	rate     int
	bytes    int64
	firstAt  time.Time
	lead     int64 // bytes of leading silence, while it is still being counted
	leadDone bool
	tail     []byte // the last ~1.5 s of PCM, for the trailing silence
}

func newSegmentProbe(rate int) *segmentProbe {
	if rate <= 0 {
		rate = 16000
	}
	return &segmentProbe{rate: rate}
}

func windowSilent(b []byte) bool {
	if len(b) < 2 {
		return true
	}
	var sum float64
	n := len(b) / 2
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8))
		sum += v * v
	}
	return sum/float64(n) < float64(segmentSilenceRMS)*float64(segmentSilenceRMS)
}

// add feeds one PCM16 mono chunk.
func (p *segmentProbe) add(chunk []byte) {
	if p.firstAt.IsZero() {
		p.firstAt = time.Now()
	}
	p.bytes += int64(len(chunk))
	if !p.leadDone {
		win := p.rate / 100 * 2 // 10 ms
		for i := 0; i+win <= len(chunk); i += win {
			if windowSilent(chunk[i : i+win]) {
				p.lead += int64(win)
			} else {
				p.leadDone = true
				break
			}
		}
		if len(chunk)%win != 0 && !p.leadDone && len(chunk) < win {
			p.lead += int64(len(chunk))
		}
	}
	p.tail = append(p.tail, chunk...)
	if max := p.rate * 2 * 3 / 2; len(p.tail) > max {
		p.tail = p.tail[len(p.tail)-max:]
	}
}

func (p *segmentProbe) ms(bytes int64) int64 { return bytes * 1000 / int64(p.rate*2) }

// leadingMs and trailingMs are the silence at the start and end of the segment's audio.
func (p *segmentProbe) leadingMs() int64 { return p.ms(p.lead) }
func (p *segmentProbe) trailingMs() int64 {
	win := p.rate / 100 * 2
	var silent int64
	for end := len(p.tail); end-win >= 0; end -= win {
		if !windowSilent(p.tail[end-win : end]) {
			break
		}
		silent += int64(win)
	}
	return p.ms(silent)
}

// logTTSSegment writes the segment's line and keeps its trailing silence for the next segment's line.
func (ms *ManagedStream) logTTSSegment(p *segmentProbe, text, lang string, idx int, bytesBefore, prevTrailMs int64, started time.Time, interrupted bool) {
	if p.bytes == 0 {
		return
	}
	turnStart := ms.ttsFirstChunkTime
	if turnStart.IsZero() || p.firstAt.Before(turnStart) {
		turnStart = p.firstAt
	}
	startMs := p.firstAt.Sub(turnStart).Milliseconds()
	prevEndMs := p.ms(bytesBefore) // where the previous segments' audio ends on a continuous playback clock, from the turn's first audio
	underrun := startMs - prevEndMs
	if underrun < 0 || idx == 0 {
		underrun = 0
	}
	trail := p.trailingMs()
	est := int64(-1)
	if idx > 0 {
		est = underrun + prevTrailMs + p.leadingMs()
	}
	ms.mu.Lock()
	ms.segPrevTrailMs = trail
	ms.mu.Unlock()
	ms.logger.Info("tts_segment", "lang", lang, "seg_idx", idx, "chars", len([]rune(text)), "audio_ms", p.ms(p.bytes),
		"synth_ms", time.Since(started).Milliseconds(), "first_audio_since_turn_start_ms", startMs, "prev_audio_end_ms", prevEndMs,
		"underrun_ms", underrun, "leading_silence_ms", p.leadingMs(), "prev_trailing_silence_ms", prevTrailMs, "trailing_silence_ms", trail,
		"est_audible_gap_ms", est, "interrupted", interrupted)
}
