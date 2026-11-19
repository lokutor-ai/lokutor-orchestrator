package orchestrator

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// toneTTS "says" each word as a harmonic tone, 0.25 s, with 0.2 s of silence after it: rising for a word ending in
// "?", falling before "...". Its visemes mark each word and each silence (none when noTimings).
type toneTTS struct {
	MockTTSProvider
	rate      int
	noTimings bool
	calls     int32
	inflight  int32
	maxIn     int32
	mu        sync.Mutex
	langs     []Language
}

func (t *toneTTS) StreamSynthesizeVisemes(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error,
	onVisemes func([]Viseme)) error {
	if !IsBackgroundSynthesis(ctx) {
		panic("the call's sounds were synthesised as if they were the reply")
	}
	atomic.AddInt32(&t.calls, 1)
	n := atomic.AddInt32(&t.inflight, 1)
	defer atomic.AddInt32(&t.inflight, -1)
	for {
		m := atomic.LoadInt32(&t.maxIn)
		if n <= m || atomic.CompareAndSwapInt32(&t.maxIn, m, n) {
			break
		}
	}
	t.mu.Lock()
	t.langs = append(t.langs, lang)
	t.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	var x []float64
	var vis []Viseme
	sil := func(s float64) {
		vis = append(vis, Viseme{ID: 0, Name: "sil", Start: float64(len(x)) / float64(t.rate), End: float64(len(x))/float64(t.rate) + s})
		x = append(x, make([]float64, int(s*float64(t.rate)))...)
	}
	sil(0.1)
	for _, w := range strings.Fields(text) {
		n := int(0.25 * float64(t.rate))
		semis := 0.0
		if strings.HasSuffix(w, "?") {
			semis = 5
		} else if strings.HasSuffix(w, "...") {
			semis = -3
		}
		vis = append(vis, Viseme{ID: 10, Name: "aa", Start: float64(len(x)) / float64(t.rate), End: float64(len(x)+n) / float64(t.rate)})
		ph := 0.0
		for i := 0; i < n; i++ {
			f0 := 180 * math.Pow(2, semis*float64(i)/float64(n)/12)
			ph += 2 * math.Pi * f0 / float64(t.rate)
			env := math.Min(1, math.Min(float64(i), float64(n-i))/(0.02*float64(t.rate)))
			x = append(x, (0.3*math.Sin(ph)+0.1*math.Sin(2*ph))*env)
		}
		sil(0.2)
	}
	if !t.noTimings && onVisemes != nil {
		onVisemes(vis)
	}
	return onChunk(floatToPCM(x))
}

func TestBackchannelSoundsMadeFromCarriers(t *testing.T) {
	tts := &toneTTS{rate: 16000}
	b, requests, err := generateBackchannelBank(WithBackgroundSynthesis(context.Background()), tts, "F1", "es", 16000)
	if err != nil || b == nil {
		t.Fatalf("bank %v err %v", b, err)
	}
	if requests != 12 || tts.calls != 12 {
		t.Fatalf("%d requests (%d calls), want 6 items x 2 takes", requests, tts.calls)
	}
	if tts.maxIn > backchannelGenInFlight {
		t.Fatalf("%d requests at once, want at most %d", tts.maxIn, backchannelGenInFlight)
	}
	for _, c := range []string{BackchannelNeutral, BackchannelAck, BackchannelSympathy, BackchannelSurprise} {
		if len(b.Clips[c]) == 0 {
			t.Fatalf("no %s sound: %v", c, len(b.Clips))
		}
		for _, clip := range b.Clips[c] {
			if d := float64(len(clip)/2) / 16000; d < 0.15 || d > 0.9 {
				t.Fatalf("%s sound %.2f s", c, d)
			}
		}
	}
}

func TestBackchannelSoundsWithoutTimingsAndWithoutCarriers(t *testing.T) {
	tts := &toneTTS{rate: 16000, noTimings: true}
	b, _, err := generateBackchannelBank(WithBackgroundSynthesis(context.Background()), tts, "F1", "sw", 16000)
	if err != nil || b == nil || len(b.Clips[BackchannelNeutral]) == 0 {
		t.Fatalf("a language with no carrier and no timings: bank %v err %v", b, err)
	}
	for _, it := range backchannelCarriers("sw") {
		first := strings.ToLower(strings.Trim(strings.Fields(it.text)[0], ",.?"))
		if first != "mhm" && first != "aha" && first != "ah" && first != "oh" && first != "mm" {
			t.Fatalf("a carrier for an unknown language starts with %q", first)
		}
	}
}

func TestSurpriseNeedsARise(t *testing.T) {
	tts := &toneTTS{rate: 16000}
	var flat, rising []byte
	bg := WithBackgroundSynthesis(context.Background())
	tts.StreamSynthesizeVisemes(bg, "Oh, mm.", "F1", "en", func(c []byte) error { flat = c; return nil }, func([]Viseme) {})
	var vis []Viseme
	tts.StreamSynthesizeVisemes(bg, "Oh? Really?", "F1", "en", func(c []byte) error { rising = c; return nil }, func(v []Viseme) { vis = v })
	x := pcmToFloat(rising)
	if ok, _, _ := scoreBackchannelSound(cutFirstSound(x, 16000, vis), x, 16000, BackchannelSurprise); !ok {
		t.Fatal("a rising 'oh?' failed as surprise")
	}
	y := pcmToFloat(flat)
	if ok, _, _ := scoreBackchannelSound(cutFirstSound(y, 16000, nil), y, 16000, BackchannelSurprise); ok {
		t.Fatal("a flat 'oh' passed as surprise")
	}
}

// The call makes its sounds from its first audio; the next call with the same voice and language takes them from the
// process's cache; a language change makes them again.
func TestBackchannelSoundsOnTheCallCachedAndRemadeForANewLanguage(t *testing.T) {
	resetBCCache()
	tts := &toneTTS{rate: 16000}
	cfg := DefaultConfig()
	cfg.Backchannels = true
	mk := func() *ManagedStream {
		orch := NewWithVAD(&MockSTTProvider{}, &bcPickLLM{}, tts, &bcVAD{}, cfg)
		ms := orch.NewManagedStream(context.Background(), NewConversationSession("gen"))
		ms.SetPlaybackRate(16000)
		orch.SetLanguage(ms.session, LanguageEs)
		return ms
	}
	wait := func(ms *ManagedStream) *BackchannelBank {
		for i := 0; i < 400; i++ {
			if b := ms.ensureBackchannelBank(); b != nil {
				return b
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
	a := mk()
	defer a.Close()
	if wait(a) == nil {
		t.Fatal("no sounds made on the call")
	}
	calls := atomic.LoadInt32(&tts.calls)
	b := mk()
	defer b.Close()
	if b.ensureBackchannelBank() == nil || atomic.LoadInt32(&tts.calls) != calls {
		t.Fatal("the next call with the same voice made its sounds again instead of using the cache")
	}
	b.orch.SetLanguage(b.session, LanguageEn)
	if wait(b) == nil || atomic.LoadInt32(&tts.calls) == calls {
		t.Fatal("a language change did not make the sounds again")
	}
	tts.mu.Lock()
	last := tts.langs[len(tts.langs)-1]
	tts.mu.Unlock()
	if last != LanguageEn {
		t.Fatalf("remade in %q, want en", last)
	}
}
