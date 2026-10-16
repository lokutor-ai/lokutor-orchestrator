package orchestrator

import (
	"context"
	"time"
)

// STTPrimer is an STT provider that can do part of an utterance's work before it is transcribed: Prime is given the
// utterance as it stands, while the caller is still speaking, and returns nothing. The transcription that follows is the
// same; only less of it is left for when the caller stops.
//
// Why (2026-10-16): the denoiser in front of recognition (lokutor_tts Psst) ran over the whole utterance when its first
// pass came, at the end of speech, so its time grew with what the caller said and sat between the caller stopping and
// the reply: recognition on the critical path was 87 ms median for utterances of 3-5 s (10% of turns), 167 ms at
// 5-8 s and 352 ms past 8 s (734 turns, 7 days). Priming every 0.5 s of speech lets that pass denoise only the rest.
type STTPrimer interface {
	Prime(ctx context.Context, audio []byte)
}

// primeEvery is how much new speech there is between primes. What is left for the pass at the end of speech is the
// speech since the last prime, so this bounds the denoising between the caller stopping and their transcript: Psst
// costs 45-50 ms per second of audio whatever the slice (1 ms more per pass; 16 and 8 kHz, measured 2026-10-16), so
// 1.5 s left up to 68 ms there (34 on average) and 0.5 s leaves up to 24 (12), for ~3% more denoising in all. A prime
// still running when the caller stops is waited for, and is a third as long too.
const primeEvery = 500 * time.Millisecond

// maybePrimeSTT primes the STT provider with the utterance so far (the lead-in and the speech, as the transcription
// passes send it) once primeEvery of new speech has arrived, one prime at a time, in the background.
func (ms *ManagedStream) maybePrimeSTT() {
	if ms.orch == nil {
		return
	}
	primer, ok := ms.orch.stt.(STTPrimer)
	if !ok {
		return
	}
	rate := ms.inputSampleRate
	if rate <= 0 {
		rate = 16000
	}
	every := int(primeEvery.Seconds()*float64(rate)) * 2
	ms.mu.Lock()
	n := len(ms.speechAudioBuf)
	if n < ms.primedLen {
		ms.primedLen = 0 // a new utterance
	}
	if ms.priming || n-ms.primedLen < every {
		ms.mu.Unlock()
		return
	}
	snapshot := make([]byte, 0, len(ms.speechLeadIn)+n)
	snapshot = append(snapshot, ms.speechLeadIn...)
	snapshot = append(snapshot, ms.speechAudioBuf...)
	ms.primedLen = n
	ms.priming = true
	ms.mu.Unlock()
	go func() {
		defer func() {
			_ = recover()
			ms.mu.Lock()
			ms.priming = false
			ms.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(ms.ctx, 5*time.Second)
		defer cancel()
		primer.Prime(ctx, snapshot)
	}()
}
