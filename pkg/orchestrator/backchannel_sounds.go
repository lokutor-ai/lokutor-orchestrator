package orchestrator

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"
)

// The call's backchannel sounds, made on the call (2026-11-19, the owner: any voice, any language, a clone the same
// minute it was made, nothing prepared beforehand).
//
// At the call's first audio the stream asks its own synthesis for a few short carrier phrases in the call's voice and
// language, with their mouth-shape timings ("Mhm, sí, te sigo.", "Oh? ¿De verdad?"), and cuts the sound out of each:
// a listener's sound said on its own comes out stretched and odd, said at the start of a phrase it has a listener's
// prosody. The cut runs from the first sound to the quietest point before the next word (the first silence the
// timings mark; without timings, the take's voiced stretches). Each take is checked (length, level, one sound and not
// part of a word, the pitch shape its category needs: surprise rising, sympathy not) and the best per item kept, at
// its category's level. 6 items x 2 takes, at most backchannelGenInFlight requests at once.
//
// The sounds live in memory for the call; a small per-process cache keyed by voice and language reuses them for the
// next call with the same voice (nothing is stored anywhere). A language change regenerates them for the new one.

const (
	backchannelGenTakes    = 2
	backchannelGenInFlight = 3
	backchannelGenTimeout  = 20 * time.Second
	backchannelCacheMax    = 64
	backchannelCacheTTL    = time.Hour
)

// backchannelWords spell the sounds and give each language its carrier words; a language not here gets carriers made of
// sounds only, and no word.
type backchannelWords struct{ mhm, aha, ah, oh, mm, yes, goOn, sorry, really string }

var backchannelLangs = map[string]backchannelWords{
	"es": {"Mhm", "Ajá", "Ah", "Oh", "Mm", "sí", "te sigo", "lo siento", "¿De verdad?"},
	"en": {"Mhm", "Aha", "Ah", "Oh", "Mm", "yeah", "go on", "I'm sorry", "Really?"},
	"ca": {"Mhm", "Ahà", "Ah", "Oh", "Mm", "sí", "t'escolto", "ho sento", "De debò?"},
	"gl": {"Mhm", "Ahá", "Ah", "Oh", "Mm", "si", "sigue", "síntoo", "De verdade?"},
	"eu": {"Mhm", "Aha", "Ah", "O", "Mm", "bai", "jarraitu", "sentitzen dut", "Benetan?"},
	"pt": {"Mhm", "Ahã", "Ah", "Oh", "Mm", "sim", "continue", "lamento", "A sério?"},
	"fr": {"Mhm", "Ah oui", "Ah", "Oh", "Mm", "oui", "je vous écoute", "je suis désolé", "Vraiment ?"},
	"it": {"Mhm", "Ah sì", "Ah", "Oh", "Mm", "sì", "la seguo", "mi dispiace", "Davvero?"},
	"de": {"Mhm", "Aha", "Ah", "Oh", "Mm", "ja", "erzählen Sie", "das tut mir leid", "Wirklich?"},
	"nl": {"Mhm", "Aha", "Ah", "O", "Mm", "ja", "ga door", "wat vervelend", "Echt?"},
	"pl": {"Mhm", "Aha", "Ach", "Och", "Mm", "tak", "słucham", "przykro mi", "Naprawdę?"},
	"ro": {"Mhm", "Aha", "A", "Oh", "Mm", "da", "continuați", "îmi pare rău", "Serios?"},
	"ru": {"Мгм", "Ага", "А", "О", "Мм", "да", "слушаю", "мне жаль", "Правда?"},
}

type backchannelItem struct {
	category string
	text     string
}

// backchannelCarriers are the call language's carrier phrases; the sound is each one's first word.
func backchannelCarriers(lang Language) []backchannelItem {
	l := strings.ToLower(string(lang))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	w, ok := backchannelLangs[l]
	if !ok {
		return []backchannelItem{
			{BackchannelNeutral, "Mhm, mhm."}, {BackchannelNeutral, "Aha, mhm."}, {BackchannelAck, "Ah, mm."},
			{BackchannelSympathy, "Oh... mm."}, {BackchannelSympathy, "Mm... mm."}, {BackchannelSurprise, "Oh? Mm."},
		}
	}
	return []backchannelItem{
		{BackchannelNeutral, w.mhm + ", " + w.yes + ", " + w.goOn + "."},
		{BackchannelNeutral, w.aha + ", " + w.goOn + "."},
		{BackchannelAck, w.ah + ", " + w.yes + ", " + w.goOn + "."},
		{BackchannelSympathy, w.oh + "... " + w.sorry + "."},
		{BackchannelSympathy, w.mm + "... " + w.sorry + "."},
		{BackchannelSurprise, w.oh + "? " + w.really},
	}
}

// backchannelTargetDB is each category's level against the voice's own speech.
var backchannelTargetDB = map[string]float64{BackchannelNeutral: -5, BackchannelAck: -4, BackchannelSympathy: -8, BackchannelSurprise: -3}

type bcCacheEntry struct {
	bank *BackchannelBank
	at   time.Time
}

var (
	bcCacheMu sync.Mutex
	bcCache   = map[string]bcCacheEntry{}
)

func bcCacheGet(key string) *BackchannelBank {
	bcCacheMu.Lock()
	defer bcCacheMu.Unlock()
	e, ok := bcCache[key]
	if !ok || time.Since(e.at) > backchannelCacheTTL {
		delete(bcCache, key)
		return nil
	}
	e.at = time.Now()
	bcCache[key] = e
	return e.bank
}

func bcCachePut(key string, b *BackchannelBank) {
	bcCacheMu.Lock()
	defer bcCacheMu.Unlock()
	if len(bcCache) >= backchannelCacheMax {
		oldest, at := "", time.Now()
		for k, e := range bcCache {
			if e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		delete(bcCache, oldest)
	}
	bcCache[key] = bcCacheEntry{bank: b, at: time.Now()}
}

// generateBackchannelBank makes the call's sounds with tts (its mouth-shape timings place the cuts). rate is the PCM's.
func generateBackchannelBank(ctx context.Context, tts VisemeTTSProvider, voice Voice, lang Language, rate int) (*BackchannelBank, int, error) {
	items := backchannelCarriers(lang)
	type take struct {
		pcm []byte
		vis []Viseme
		err error
	}
	takes := make([][]take, len(items))
	for i := range takes {
		takes[i] = make([]take, backchannelGenTakes)
	}
	sem := make(chan struct{}, backchannelGenInFlight)
	var wg sync.WaitGroup
	for i := range items {
		for t := 0; t < backchannelGenTakes; t++ {
			wg.Add(1)
			go func(i, t int) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					takes[i][t].err = ctx.Err()
					return
				}
				defer func() { <-sem }()
				var buf []byte
				var vis []Viseme
				err := tts.StreamSynthesizeVisemes(ctx, items[i].text, voice, lang,
					func(c []byte) error { buf = append(buf, c...); return nil },
					func(v []Viseme) { vis = append(vis, v...) })
				takes[i][t] = take{pcm: buf, vis: vis, err: err}
			}(i, t)
		}
	}
	wg.Wait()
	out := &BackchannelBank{SampleRate: rate, Clips: map[string][][]byte{}}
	var firstErr error
	requests := 0
	for i, it := range items {
		var best []byte
		bestScore := math.Inf(-1)
		for _, tk := range takes[i] {
			requests++
			if tk.err != nil {
				if firstErr == nil {
					firstErr = tk.err
				}
				continue
			}
			x := pcmToFloat(tk.pcm)
			clip := cutFirstSound(x, rate, tk.vis)
			if clip == nil {
				continue
			}
			ok, score, levelled := scoreBackchannelSound(clip, x, rate, it.category)
			if ok && score > bestScore {
				best, bestScore = floatToPCM(levelled), score
			}
		}
		if best != nil {
			out.Clips[it.category] = append(out.Clips[it.category], best)
		}
	}
	if len(out.Clips) == 0 {
		return nil, requests, firstErr
	}
	return out, requests, nil
}

func pcmToFloat(b []byte) []float64 {
	x := make([]float64, len(b)/2)
	for i := range x {
		x[i] = float64(int16(uint16(b[2*i])|uint16(b[2*i+1])<<8)) / 32768
	}
	return x
}

func floatToPCM(x []float64) []byte {
	b := make([]byte, 2*len(x))
	for i, v := range x {
		s := int16(math.Max(-32768, math.Min(32767, math.Round(v*32767))))
		b[2*i], b[2*i+1] = byte(s), byte(uint16(s)>>8)
	}
	return b
}

func frameRMS(x []float64, rate int) ([]float64, int) {
	hop := rate / 100
	if hop < 1 {
		hop = 1
	}
	var f []float64
	for i := 0; i+hop <= len(x); i += hop {
		var s float64
		for _, v := range x[i : i+hop] {
			s += v * v
		}
		f = append(f, math.Sqrt(s/float64(hop)))
	}
	return f, hop
}

func maxOf(f []float64) float64 {
	m := 0.0
	for _, v := range f {
		if v > m {
			m = v
		}
	}
	return m
}

// voicedRMS is the RMS over the 10 ms frames within 20 dB of the loudest: the level of the speech, not of its pauses.
func voicedRMS(x []float64, rate int) float64 {
	f, _ := frameRMS(x, rate)
	m := maxOf(f)
	var s float64
	n := 0
	for _, v := range f {
		if v >= m*0.1 {
			s += v * v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(s / float64(n))
}

// firstSoundEnd is where the take's first sound ends and the next starts (seconds): the first silence the mouth-shape
// timings mark after speech, or without timings the take's first gap of 80 ms or more in its voiced frames.
func firstSoundEnd(x []float64, rate int, vis []Viseme) (end, next float64, ok bool) {
	spoke := false
	for _, v := range vis {
		silent := v.ID == 0 || strings.EqualFold(v.Name, "sil")
		if !silent {
			if spoke && end > 0 {
				return end, v.Start, true
			}
			spoke = true
			continue
		}
		if spoke && v.End-v.Start >= 0.05 && end == 0 {
			end = v.Start
		}
	}
	f, _ := frameRMS(x, rate)
	m := maxOf(f)
	if m <= 0 {
		return 0, 0, false
	}
	i := 0
	for i < len(f) && f[i] < m*0.08 {
		i++
	}
	for i < len(f) {
		if f[i] < m*0.08 {
			j := i
			for j < len(f) && f[j] < m*0.08 {
				j++
			}
			if j-i >= 8 && j < len(f) {
				return float64(i) / 100, float64(j) / 100, true
			}
			i = j
			continue
		}
		i++
	}
	return 0, 0, false
}

// cutFirstSound is the take's first sound: from its onset to the quietest 10 ms before the next word, trimmed to its
// voiced part, 15 ms fades. Nil when the take has no clear first sound.
func cutFirstSound(x []float64, rate int, vis []Viseme) []float64 {
	end, next, ok := firstSoundEnd(x, rate, vis)
	if !ok || next <= end {
		return nil
	}
	a, b := int(end*float64(rate)), int(next*float64(rate))
	if b > len(x) {
		b = len(x)
	}
	hop := rate / 100
	at := b
	if b-a > hop {
		best := math.Inf(1)
		for i := a; i+hop <= b; i += hop / 2 {
			var s float64
			for _, v := range x[i : i+hop] {
				s += v * v
			}
			if s < best {
				best, at = s, i+hop/2
			}
		}
	}
	if min := a + rate*6/100; at < min && min <= b {
		at = min
	}
	seg := x[:at]
	f, h := frameRMS(seg, rate)
	m := maxOf(f)
	if m <= 0 {
		return nil
	}
	on, off := -1, -1
	for i, v := range f {
		if v >= m*0.06 {
			if on < 0 {
				on = i
			}
			off = i
		}
	}
	s, e := on*h-rate/50, (off+1)*h+rate*3/100
	if s < 0 {
		s = 0
	}
	if e > len(seg) {
		e = len(seg)
	}
	out := append([]float64(nil), seg[s:e]...)
	fade := rate * 15 / 1000
	if len(out) <= 2*fade {
		return nil
	}
	for i := 0; i < fade; i++ {
		g := float64(i) / float64(fade)
		out[i] *= g
		out[len(out)-1-i] *= g
	}
	return out
}

// scoreBackchannelSound checks a cut sound (the module comment) and returns it at its category's level.
func scoreBackchannelSound(clip, take []float64, rate int, category string) (bool, float64, []float64) {
	dur := float64(len(clip)) / float64(rate)
	ref := voicedRMS(take, rate)
	lvl := voicedRMS(clip, rate)
	if ref <= 0 || lvl <= 0 {
		return false, 0, nil
	}
	levelDB := 20 * math.Log10(lvl/ref)
	maxDur := 0.7
	if category == BackchannelSympathy {
		maxDur = 0.9
	}
	gapMax := 0.06
	if category == BackchannelNeutral {
		gapMax = 0.13 // aha, mm-hm dip in the middle
	}
	slope, voiced := pitchSlopeSemitones(clip, rate)
	ok := dur >= 0.15 && dur <= maxDur && levelDB >= -14 && levelDB <= 6 && longestGap(clip, rate) <= gapMax &&
		crossingsPerSecond(clip, rate) <= 2500
	switch category {
	case BackchannelSurprise:
		ok = ok && voiced && slope >= 1
	case BackchannelSympathy:
		ok = ok && (!voiced || slope <= 0.5)
	}
	score := -math.Abs(dur-0.32) - math.Abs(levelDB)/20
	if category == BackchannelSurprise && voiced {
		score += math.Min(slope, 4) / 4
	}
	g := math.Pow(10, (backchannelTargetDB[category]-levelDB)/20)
	out := make([]float64, len(clip))
	pk := 0.0
	for i, v := range clip {
		out[i] = v * g
		pk = math.Max(pk, math.Abs(out[i]))
	}
	if pk > 0.89 {
		for i := range out {
			out[i] *= 0.89 / pk
		}
	}
	return ok, score, out
}

func longestGap(x []float64, rate int) float64 {
	f, _ := frameRMS(x, rate)
	m := maxOf(f)
	best, run := 0, 0
	for _, v := range f {
		if v < m*0.1 {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	return float64(best) / 100
}

// crossingsPerSecond over the voiced frames: hums and vowels cross zero a few hundred times a second, fricatives and
// bursts (part of a word) thousands.
func crossingsPerSecond(x []float64, rate int) float64 {
	f, hop := frameRMS(x, rate)
	m := maxOf(f)
	n, frames := 0, 0
	for k, v := range f {
		if v < m*0.2 {
			continue
		}
		frames++
		seg := x[k*hop : k*hop+hop]
		for i := 1; i < len(seg); i++ {
			if (seg[i-1] < 0) != (seg[i] < 0) {
				n++
			}
		}
	}
	if frames == 0 {
		return 0
	}
	return float64(n) / (float64(frames) / 100)
}

// pitchSlopeSemitones is how far the pitch moves over the sound: the sum of the frame-to-frame steps of its voiced F0
// (autocorrelation on the clip at a quarter of its rate), steps over 3 semitones left out (the tracker an octave off).
// voiced false when too little of it is voiced to say.
func pitchSlopeSemitones(x []float64, rate int) (float64, bool) {
	dec := 1
	for rate/dec > 12000 {
		dec++
	}
	sr := rate / dec
	y := make([]float64, len(x)/dec)
	for i := range y {
		var s float64
		for j := 0; j < dec; j++ {
			s += x[i*dec+j]
		}
		y[i] = s / float64(dec)
	}
	win, hop := sr*40/1000, sr/100
	lo, hi := sr/450, sr/70
	f, _ := frameRMS(y, sr)
	peak := maxOf(f)
	var st []float64
	for i := 0; i+win+hi < len(y); i += hop {
		seg := y[i : i+win]
		var mean, e float64
		for _, v := range seg {
			mean += v
		}
		mean /= float64(win)
		for _, v := range seg {
			e += (v - mean) * (v - mean)
		}
		if math.Sqrt(e/float64(win)) < 0.15*peak || e <= 0 {
			continue
		}
		bestK, best := 0, 0.0
		for k := lo; k < hi; k++ {
			var c float64
			for j := 0; j < win; j++ {
				c += (y[i+j] - mean) * (y[i+j+k] - mean)
			}
			if c > best {
				best, bestK = c, k
			}
		}
		if bestK == 0 || best/e < 0.45 {
			continue
		}
		st = append(st, 12*math.Log2(float64(sr)/float64(bestK)))
	}
	if len(st) < 6 {
		return 0, false
	}
	var sum float64
	for i := 1; i < len(st); i++ {
		if d := st[i] - st[i-1]; math.Abs(d) <= 3 {
			sum += d
		}
	}
	return sum, true
}
