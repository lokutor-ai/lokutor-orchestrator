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
// The key dropped `language` when the phrases became language-agnostic. They are nasal hums
// synthesised unmarked (see backchannelPhrases), so the audio for a given voice is now identical
// whatever language the call is in — keeping language in the key would have meant synthesising the
// same three clips again for every one of nine languages, which is exactly the warm-up cost this
// cache exists to remove.

type backchannelKey struct {
	voice Voice
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

// cachedBackchannelClips returns the clips for a voice, synthesising them once per process. gen is
// only called on the first request for a key; concurrent callers wait for that one result.
func cachedBackchannelClips(ctx context.Context, voice Voice, gen func(context.Context) [][]byte) [][]byte {
	key := backchannelKey{voice: voice}

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
