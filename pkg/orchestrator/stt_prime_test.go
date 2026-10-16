package orchestrator

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// primingSTT records what it was primed with.
type primingSTT struct {
	MockSTTProvider
	mu     sync.Mutex
	primes [][]byte
	block  chan struct{}
}

func (p *primingSTT) Prime(ctx context.Context, audio []byte) {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.primes = append(p.primes, append([]byte(nil), audio...))
	p.mu.Unlock()
}

func (p *primingSTT) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.primes) }

// The provider is primed with the lead-in and the speech once primeEvery of new speech has arrived, one prime at a time,
// and the count starts again with the next utterance.
func TestSTTIsPrimedEveryStretchOfSpeech(t *testing.T) {
	stt := &primingSTT{MockSTTProvider: MockSTTProvider{transcribeResult: "x"}, block: make(chan struct{})}
	cfg := DefaultConfig()
	cfg.SampleRate = 16000
	orch := NewWithAllLayers(stt, &chunkedLLM{}, &countingTTS{}, nil, cfg, &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("prime"))
	defer ms.Close()

	step := int(primeEvery.Seconds()*16000) * 2 // primeEvery of 16 kHz PCM16
	sec := step * 2 / 3                         // two thirds of it: not yet
	ms.mu.Lock()
	ms.speechLeadIn = bytes.Repeat([]byte{1}, 100)
	ms.speechAudioBuf = bytes.Repeat([]byte{2}, sec)
	ms.mu.Unlock()
	ms.maybePrimeSTT()
	time.Sleep(20 * time.Millisecond)
	if stt.count() != 0 || ms.priming {
		t.Fatal("primed before primeEvery of speech")
	}

	ms.mu.Lock()
	ms.speechAudioBuf = append(ms.speechAudioBuf, bytes.Repeat([]byte{3}, sec)...) // 4/3 of primeEvery
	ms.mu.Unlock()
	ms.maybePrimeSTT()
	ms.mu.Lock()
	ms.speechAudioBuf = append(ms.speechAudioBuf, bytes.Repeat([]byte{4}, 2*sec)...) // 8/3, but a prime is running
	ms.mu.Unlock()
	ms.maybePrimeSTT()
	close(stt.block)
	deadline := time.Now().Add(time.Second)
	for stt.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	stt.mu.Lock()
	if len(stt.primes) != 1 || len(stt.primes[0]) != 100+2*sec || stt.primes[0][0] != 1 || stt.primes[0][100] != 2 {
		t.Fatalf("primes %d, first %d bytes", len(stt.primes), len(stt.primes[0]))
	}
	stt.mu.Unlock()

	ms.maybePrimeSTT() // 4/3 of primeEvery new since the last prime: primes again, with everything
	for stt.count() < 2 && time.Now().Before(deadline.Add(time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
	stt.mu.Lock()
	if len(stt.primes) != 2 || len(stt.primes[1]) != 100+4*sec {
		t.Fatalf("second prime: %d primes", len(stt.primes))
	}
	stt.mu.Unlock()

	ms.mu.Lock()
	ms.speechAudioBuf = bytes.Repeat([]byte{5}, sec/2) // a new utterance, a third of primeEvery
	ms.mu.Unlock()
	ms.maybePrimeSTT()
	time.Sleep(20 * time.Millisecond)
	if stt.count() != 2 {
		t.Fatal("a new utterance was primed before primeEvery of it")
	}
}
