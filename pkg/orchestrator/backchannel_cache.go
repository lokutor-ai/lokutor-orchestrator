package orchestrator

import (
	"context"
	"sync"
	"time"
)

// Backchannel clips are the same audio every time, so synthesise them once per process.
//
// They were generated per ManagedStream — a goroutine fired at session creation that synthesised
// several short phrases, and again on every voice or language change. The phrases are a fixed list
// and the voice and language are the only inputs, so every session after the first was re-making
// audio byte-for-byte identical to audio it already had.
//
// That was not free. Production logs caught it directly: a burst of 2-6 character syntheses at
// session start, and the caller's first real sentence immediately after at RTF 1.311 — over
// real-time, so the audio stream underran and the caller heard a GAP. The worker runs on four
// cores and admits two concurrent syntheses, so warm-up work does not wait politely behind live
// speech; it takes a slot. The provider saw first audio 1.2-1.7s after requesting it while the
// engine itself reported 250-400ms, the difference being time spent queueing behind these clips.
//
// Caching keyed on the voice removes all of it after the first session, and the in-flight guard
// means ten simultaneous sessions on a cold process generate one set rather than ten.
//
// The key dropped `language` once, on the reasoning that the phrases are nasal hums and so the
// audio for a given voice is identical whatever language the call is in. That was wrong, and a
// caller heard it: the clips came out in a different speaker's voice from the reply.
//
// The phrase text is language-agnostic; the SYNTHESIS is not. Versa's resolveVoice picks the
// reference pack from the language — resolveVoice("F1", "es") is es_f1 and resolveVoice("F1",
// "en") is en_f1, different speakers — and essentially every agent in the database still stores a
// legacy F1-F5 / M1-M5 name, so essentially every call took that branch. Synthesising the clips
// under a fixed language therefore picked a fixed pack while the reply followed the caller's,
// which is a different human humming between the agent's own sentences.
//
// So language is back in the key, and it costs almost nothing: a call has one language, not nine,
// so this is three clips per (voice, language) actually used, once per process.

type backchannelKey struct {
	voice Voice
	lang  Language
}

type backchannelEntry struct {
	once  sync.Once
	clips [][]byte
	done  chan struct{}
}

var (
	backchannelMu    sync.Mutex
	backchannelCache = map[backchannelKey]*backchannelEntry{}
)

// backchannelGenTimeout bounds one warm-up. It is background work: if the box is busy enough that
// this does not finish, the right outcome is to give up and leave the session without clips, not
// to keep competing with whatever is making it slow.
const backchannelGenTimeout = 20 * time.Second

// cachedBackchannelClips returns the clips for a (voice, language), synthesising them once per
// process. gen is only called on the first request for a key; concurrent callers wait for that one
// result.
func cachedBackchannelClips(ctx context.Context, voice Voice, lang Language, gen func(context.Context) [][]byte) [][]byte {
	key := backchannelKey{voice: voice, lang: lang}

	backchannelMu.Lock()
	e, ok := backchannelCache[key]
	if !ok {
		e = &backchannelEntry{done: make(chan struct{})}
		backchannelCache[key] = e
	}
	backchannelMu.Unlock()

	go e.once.Do(func() {
		defer close(e.done)
		// Deliberately not the session's context: the clips outlive the session that happened to
		// ask for them, and a caller hanging up mid-generation should not leave the cache entry
		// permanently empty for everyone else.
		c, cancel := context.WithTimeout(context.Background(), backchannelGenTimeout)
		defer cancel()
		e.clips = gen(c)
	})

	select {
	case <-e.done:
		return e.clips
	case <-ctx.Done():
		return nil // session ended first; the generation continues and lands in the cache
	}
}
