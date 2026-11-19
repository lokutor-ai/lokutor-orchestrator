package orchestrator

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A reply's very short first sentence is synthesised together with the next one (2026-11-19, the owner's choice).
//
// With reference continuation, every sentence of a reply after the first is appended to one running stream. When the
// first sentence is a lone "Vale." / "Claro." / "Vale, seguimos.", the voice model (the HireVoicer clone most of all)
// treats the appended next sentence as more of the same: it says "vale" again and stops, and the real sentence is never
// spoken. The TTS session reproduced it offline in the production layout (prod_regress H080, H082, H083, H090; the
// owner's own call "Vale…" then nothing). Sent as ONE request, the opener and its sentence are one unit for the model,
// as a person says them. This only changes how the reply's text is grouped into synthesis requests; nothing about the
// model's output is filtered or edited.
//
// Only the FIRST chunk of a reply is affected. The opener waits at most shortOpenerWait for the next sentence; if the
// reply ends first, or nothing arrives in time, or the turn is cancelled, it is spoken alone, so nothing is lost and the
// added delay is bounded. Across 921 production turns of the last 7 days the second sentence's audio followed the
// first's by 172/374 ms p50/p90, so the wait is usually far below the cap; 139 of 1,189 replies (12%) opened with a
// chunk this short.
var (
	shortOpenerMinChars = envIntDefault("TTS_MIN_FIRST_CHUNK_CHARS", 16) // fewer runes than this = a short opener; 0 = off
	shortOpenerWait     = time.Duration(envIntDefault("TTS_SHORT_OPENER_WAIT_MS", 800)) * time.Millisecond
)

func envIntDefault(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// isShortOpener reports whether a reply's first chunk is short enough to be spoken with the sentence after it.
func isShortOpener(text string) bool {
	t := strings.TrimSpace(text)
	if shortOpenerMinChars <= 0 || t == "" {
		return false
	}
	// Under 16 characters ("Vale.", "Perfecto.", "Vale, seguimos.") or at most two words ("Perfecto, gracias."):
	// the TTS session's offline check on the clone's failing replies (16 sentences) went 6 failures with no merge,
	// 1 merging under 12 characters, 0 under 16.
	return utf8.RuneCountInString(t) < shortOpenerMinChars || len(strings.Fields(t)) <= 2
}

// joinShortOpener returns the reply's first chunk, joined with the next one from queue when the first is a short
// opener and the next arrives within wait; merged reports whether it was joined.
func joinShortOpener(ctx context.Context, first string, queue <-chan string, wait time.Duration) (text string, merged bool) {
	if !isShortOpener(first) {
		return first, false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case next, ok := <-queue:
		if !ok || strings.TrimSpace(next) == "" {
			return first, false
		}
		return strings.TrimSpace(first) + " " + strings.TrimSpace(next), true
	case <-t.C:
		return first, false
	case <-ctx.Done():
		return first, false
	}
}

// joinShortOpenerSegments is the same rule for a reply whose sentences are all known at once (a speculative hit, a
// non-streaming answer): the first segment is merged with the second when it is a short opener.
func joinShortOpenerSegments(segs []string) []string {
	if len(segs) < 2 || !isShortOpener(segs[0]) {
		return segs
	}
	out := make([]string, 0, len(segs)-1)
	out = append(out, strings.TrimSpace(segs[0])+" "+strings.TrimSpace(segs[1]))
	return append(out, segs[2:]...)
}
