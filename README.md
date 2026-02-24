# Lokutor Orchestrator

**High-performance voice orchestration engine for building AI-driven voice agents.**

[![Go Reference](https://pkg.go.dev/badge/github.com/lokutor-ai/lokutor-orchestrator.svg)](https://pkg.go.dev/github.com/lokutor-ai/lokutor-orchestrator)
[![Go Report Card](https://goreportcard.com/badge/github.com/lokutor-ai/lokutor-orchestrator)](https://goreportcard.com/report/github.com/lokutor-ai/lokutor-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

Lokutor Orchestrator is a production-grade Go library for building voice-powered applications. It handles the complex lifecycle of voice interactions—bridging Speech-to-Text (STT), Large Language Models (LLM), and Text-to-Speech (TTS) into a seamless, low-latency experience.

---

## Features

- Full-duplex voice orchestration (v1.3): real-time capture and playback with native 44.1kHz 16-bit PCM.
- Barge-in support: interrupts the agent promptly when the user begins speaking.
- Predictive audio buffering: prevents clipping of the start of user speech.
- High-performance echo suppression: correlation filters reduce self-interruption.
- Pluggable architecture: swap STT, LLM, and TTS implementations with minimal changes.
- Instrumentation: stage-by-stage latency tracking (STT, LLM, TTS, end-to-end).

---

## Quick Start

### 1. Installation

```bash
go get github.com/lokutor-ai/lokutor-orchestrator
```

### 2. Run the Example Agent (CLI Demo)

1.  **Configure environment:** Create a `.env` file in the root:
    ```env
    STT_PROVIDER=groq|openai|deepgram|assemblyai
    LLM_PROVIDER=groq|openai|anthropic|google
    
    GROQ_API_KEY=your_key
    OPENAI_API_KEY=your_key
    LOKUTOR_API_KEY=your_key
    AGENT_LANGUAGE=es # en, fr, de, etc.
    ```

2.  **Run the agent:**
    ```bash
    go run cmd/agent/main.go
    ```

### 3. Basic Library Usage (`ManagedStream`)

```go
func main() {
    // Initialize High-Performance Providers
    stt := sttProvider.NewDeepgramSTT(apiKey)
    llm := llmProvider.NewGroqLLM(apiKey, "llama-3.3-70b-versatile")
    tts := ttsProvider.NewLokutorTTS(apiKey)
    
    // Configure VAD & Orchestrator
    vad := orchestrator.NewRMSVAD(0.02, 150*time.Millisecond)
    orch := orchestrator.NewWithVAD(stt, llm, tts, vad, orchestrator.DefaultConfig())
    
    // Start a duplex managed stream
    session := orch.NewSessionWithDefaults("session_01")
    stream := orch.NewManagedStream(context.Background(), session)
    
    // Listen for events
    for event := range stream.Events() {
        switch event.Type {
        case orchestrator.UserSpeaking:
            stopSpeaker() // Fast barge-in
        case orchestrator.AudioChunk:
            playChunk(event.Data.([]byte))
        }
    }
}
```

---

## Provider Ecosystem

Lokutor supports all major infrastructure providers out of the box:

- **LLM**: Groq (Llama), OpenAI (GPT-4), Anthropic (Claude), Google (Gemini)
- **STT**: Groq (Whisper), OpenAI (Whisper), Deepgram (Nova-2), AssemblyAI
- **TTS**: Lokutor (Versa - optimized for minimal Time-To-First-Byte)

---

## Architecture

```
┌─────────────┐
│  Raw Mic In │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────┐
│   Lokutor ManagedStream         │
│  ┌────────────┐   ┌──────────┐  │
│  │ Echo Guard │──▶│ VAD     │  │
│  └────────────┘   └──────────┘  │
│          │             │        │
│          ▼             ▼        │
│  ┌────────────┐   ┌──────────┐  │
│  │ STT Stream │◀──│ Buffers │  │
│  └────────────┘   └──────────┘  │
│          │             │        │
│          ▼             ▼        │
│  ┌────────────┐   ┌──────────┐  │
│  │ LLM Logic  │──▶│ TTS Gen │─┐│
│  └────────────┘   └──────────┘ ││
└────────────────────────│────────┘
                         │
                         ▼
               ┌───────────────────┐
               │ Adaptive Output   │
               └───────────────────┘
```

---

## Strategies for High-Quality Interactions

Recommendations to improve conversational quality:

1. Use short filler utterances when model latency exceeds a threshold to maintain user engagement.
2. Include prosody markers in system prompts to enable dynamic TTS adjustments.
3. Use brief backchannel confirmations during extended user turns to indicate attention.
4. Acknowledge interruptions gracefully to preserve conversational continuity.

---

## Technical Details

### Echo Suppression
The orchestrator tracks every sample sent to the speaker and uses sliding-window correlation search on mic input. This prevents "self-interruption" by identifying when the mic hears the agent's own voice.

### Latency Breakdown
Every turn includes detailed instrumentation available via `stream.GetLatencyBreakdown()`:
*   `User-to-STT`: Time from user stop to final transcript.
*   `TTFB`: User stop to first audio sample.
*   `E2E`: Full user-to-speaker turn-around.

---

## License

MIT. Built with ❤️ by the Lokutor AI team.
