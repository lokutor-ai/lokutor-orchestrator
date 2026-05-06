# Noise Filter Integration for Lokutor TTS

## Overview

This package adds real-time noise suppression to the Lokutor TTS orchestrator using the DeepFilterNet V2 ONNX model.

## Files Added

```
lokutor-orchestrator/pkg/providers/noise/
  const.go        - Model configuration (v2 params)
  filterbank.go   - Bark-scale filterbank computation
  onnx.go         - ONNX runtime wrapper
  filter.go       - Real-time streaming noise filter
  wrapper.go      - STT provider wrapper
```

## Integration

### 1. Add Dependencies

In `lokutor-orchestrator/go.mod`, add:

```go
require (
    github.com/yalue/onnxruntime_go v1.12.0
    gonum.org/v1/gonum v0.15.0
)
```

Then run:
```bash
cd lokutor-orchestrator && go mod tidy
```

### 2. Install ONNX Runtime

The ONNX runtime shared library must be available. On the deployment machine:

```bash
# Download ONNX Runtime
wget https://github.com/microsoft/onnxruntime/releases/download/v1.18.0/onnxruntime-osx-arm64-1.18.0.tgz
tar xzf onnxruntime-osx-arm64-1.18.0.tgz
sudo cp onnxruntime-osx-arm64-1.18.0/lib/libonnxruntime.so.1.18.0 /usr/local/lib/libonnxruntime.so
```

For RunPod (Linux x86_64):
```bash
wget https://github.com/microsoft/onnxruntime/releases/download/v1.18.0/onnxruntime-linux-x64-1.18.0.tgz
tar xzf onnxruntime-linux-x64-1.18.0.tgz
sudo cp onnxruntime-linux-x64-1.18.0/lib/libonnxruntime.so.1.18.0 /usr/local/lib/libonnxruntime.so
```

### 3. Copy ONNX Model

Place the exported ONNX model in your deployment:
```bash
cp /path/to/noise_suppressor_v2.onnx /deploy/path/models/noise_suppressor_v2.onnx
```

### 4. Wire Into Orchestrator

In `lokutor-orchestrator/cmd/agent/main.go` (or wherever the orchestrator is initialized):

```go
import (
    "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/noise"
    "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
)

func main() {
    // ... existing setup ...

    // Create base STT provider
    baseSTT := stt.NewDeepgramSTT(cfg.DeepgramAPIKey)

    // Wrap with noise filter
    filteredSTT, err := noise.NewSTTWrapper(baseSTT, "/path/to/noise_suppressor_v2.onnx")
    if err != nil {
        log.Printf("Warning: could not create noise filter: %v", err)
        // Fall back to unfiltered STT
        filteredSTT = baseSTT
    }
    defer filteredSTT.Destroy()

    // Pass filteredSTT to orchestrator
    orch := orchestrator.NewOrchestrator(filteredSTT, ttsProvider, llmProvider)
}
```

### 5. Alternative: Apply in Orchestrator.Transcribe

If you prefer to apply filtering in the orchestrator itself instead of wrapping the provider:

In `lokutor-orchestrator/pkg/orchestrator/orchestrator.go`:

```go
type Orchestrator struct {
    stt         STTProvider
    tts         TTSProvider
    llm         LLMProvider
    noiseFilter *noise.Filter  // Add this
}

func NewOrchestrator(stt STTProvider, tts TTSProvider, llm LLMProvider) *Orchestrator {
    o := &Orchestrator{stt: stt, tts: tts, llm: llm}
    
    // Initialize noise filter (optional)
    if filter, err := noise.NewFilter("models/noise_suppressor_v2.onnx"); err == nil {
        o.noiseFilter = filter
    }
    
    return o
}

func (o *Orchestrator) Transcribe(ctx context.Context, audioData []byte, lang Language) (TranscriptionResult, error) {
    // Apply noise filter if available
    if o.noiseFilter != nil {
        samples := noise.PCMBytesToFloat32(audioData)
        cleanSamples := o.noiseFilter.ProcessChunk(samples)
        cleanSamples = append(cleanSamples, o.noiseFilter.Flush()...)
        audioData = noise.Float32ToPCMBytes(cleanSamples)
    }
    
    return o.stt.Transcribe(ctx, audioData, lang)
}
```

## Usage Notes

- **Latency:** The filter adds ~32ms algorithmic delay (512 samples at 16kHz) + processing time (~0.01ms/frame on M4, ~2ms on x86).
- **Sample Rate:** Input must be 16kHz mono PCM. If your audio is 44.1kHz, resample before filtering.
- **Memory:** The filter uses ~2MB RAM + ONNX runtime overhead.
- **Graceful Degradation:** If the ONNX model fails to load, the wrapper falls back to unfiltered STT.

## Testing

```bash
cd lokutor-orchestrator
go test ./pkg/providers/noise/...
```
