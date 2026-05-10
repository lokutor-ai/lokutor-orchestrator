package orchestrator

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	captureMu    sync.Mutex
	captureCount int
)

func captureSTTAudio(audio []byte, sampleRate int, label string) {
	if len(audio) < 44 {
		return
	}

	captureMu.Lock()
	captureCount++
	n := captureCount
	captureMu.Unlock()

	dir := "stt_captures"
	os.MkdirAll(dir, 0755)

	ts := time.Now().Format("150405.000")
	filename := filepath.Join(dir, fmt.Sprintf("stt_%s_%02d_%s.wav", ts, n, label))

	f, err := os.Create(filename)
	if err != nil {
		fmt.Printf("[AUDIOCAPTURE] failed to create %s: %v\n", filename, err)
		return
	}
	defer f.Close()

	dataSize := len(audio)
	riffSize := 36 + dataSize

	header := make([]byte, 44)
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(riffSize))
	copy(header[8:12], []byte("WAVE"))
	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))

	if _, err := f.Write(header); err != nil {
		fmt.Printf("[AUDIOCAPTURE] write header err: %v\n", err)
		return
	}
	if _, err := f.Write(audio); err != nil {
		fmt.Printf("[AUDIOCAPTURE] write audio err: %v\n", err)
		return
	}
	fmt.Printf("[AUDIOCAPTURE] saved %s (%d bytes)\n", filename, len(audio))
}
