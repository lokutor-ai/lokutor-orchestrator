package orchestrator

import (
	"bytes"
	"math/rand"
	"testing"
)

// A one-sample chunk at a rate below 16 kHz used to index audio[-2] and panic.
func TestResampleTo16kOneSampleDoesNotPanic(t *testing.T) {
	for _, in := range [][]byte{{0x34, 0x12}, {0x34, 0x12, 0x99}} {
		out := resampleTo16k(in, 8000)
		if len(out) != 4 {
			t.Fatalf("1 sample at 8 kHz -> %d bytes, want 4", len(out))
		}
		if !bytes.Equal(out, []byte{0x34, 0x12, 0x34, 0x12}) {
			t.Fatalf("1 sample at 8 kHz -> %v, want the sample held", out)
		}
	}
}

// Pins the output of every other input to what it was before the one-sample guard (a reference copy of the
// unguarded loop): the guard changes no bytes the echo check or Turno already received.
func TestResampleTo16kUnchangedForTwoOrMoreSamples(t *testing.T) {
	ref := func(audio []byte, rate int) []byte {
		if rate == 16000 {
			return audio
		}
		ratio := float64(16000) / float64(rate)
		outLen := int(float64(len(audio)/2) * ratio * 2)
		if outLen%2 != 0 {
			outLen--
		}
		if outLen <= 0 {
			return audio
		}
		out := make([]byte, outLen)
		n := len(audio) / 2
		for i := 0; i < outLen/2; i++ {
			pos := float64(i) / ratio
			idx := int(pos)
			frac := pos - float64(idx)
			if idx >= n-1 {
				idx = n - 2
			}
			s0 := int16(audio[idx*2]) | int16(audio[idx*2+1])<<8
			s1 := int16(audio[(idx+1)*2]) | int16(audio[(idx+1)*2+1])<<8
			v := int16(float64(s0)*(1-frac) + float64(s1)*frac)
			out[i*2], out[i*2+1] = byte(v), byte(v>>8)
		}
		return out
	}
	r := rand.New(rand.NewSource(7))
	for _, rate := range []int{8000, 22050, 24000, 44100, 48000} {
		for trial := 0; trial < 300; trial++ {
			n := 4 + 2*r.Intn(3000)
			in := make([]byte, n)
			r.Read(in)
			if !bytes.Equal(resampleTo16k(in, rate), ref(in, rate)) {
				t.Fatalf("rate %d, %d bytes: output changed", rate, n)
			}
		}
	}
}
