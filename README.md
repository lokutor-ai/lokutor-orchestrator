# Lokutor Orchestrator

**The open-source Go engine behind Lokutor's voice agents: turn-taking, barge-in, tool calls and streaming between the speech-to-text, language model and text-to-speech you choose.**

[![Go Reference](https://pkg.go.dev/badge/github.com/lokutor-ai/lokutor-orchestrator.svg)](https://pkg.go.dev/github.com/lokutor-ai/lokutor-orchestrator)
[![Go Report Card](https://goreportcard.com/badge/github.com/lokutor-ai/lokutor-orchestrator)](https://goreportcard.com/report/github.com/lokutor-ai/lokutor-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

This is the orchestration layer that runs Lokutor's production voice agents, published as a Go library. It ties speech recognition, a language model and a voice into one low-latency conversation, and handles the parts that are hard to get right: knowing when the caller has finished, letting them interrupt, calling tools mid-sentence and keeping the audio clean.

It works with any provider that implements its three small interfaces. Lokutor's own models and cloud are one option, not a requirement.

## Part of Lokutor

[Lokutor](https://lokutor.com) is a Madrid company that designs and trains its own speech models and runs voice agents on them: **Versa** (voice), **Turno** (turn-taking), **Psst** (noise suppression) and **Conv** (recognition). A voice agent costs about 5¢ a minute with the language model included, in the cloud, on your own servers or on a device.

- Try the voice demo, no sign-up: [lokutor.com](https://lokutor.com)
- The models and how they were measured: [lokutor.com/models](https://lokutor.com/models/) and [lokutor.com/benchmarks](https://lokutor.com/benchmarks/)
- API, SDKs and the rate card: [docs.lokutor.com](https://docs.lokutor.com)
- On-premise (the full models on your servers): [lokutor.com/on-premise-voice-ai](https://lokutor.com/on-premise-voice-ai/)
- For devices: [Oído](https://github.com/lokutor-ai/oido) (speech recognition for a $5 ESP32-S3) and [Ito](https://github.com/lokutor-ai/ito) (a voice for it), both open source

The models and the hosted platform are separate from this repository. This repository is only the orchestrator, and it is MIT licensed.

---

## What it does

- **Full-duplex managed streams:** microphone audio in, agent audio out, one event stream for everything in between.
- **Barge-in:** the agent stops when the caller starts speaking, and can resume after an interruption that was not real.
- **Layered end-of-turn detection:** voice-activity detection, lexical checks on the transcript and, when you provide it, the Turno turn-taking model. Turno's weights are not distributed in this repository; without them the orchestrator uses its VAD and lexical checks.
- **Echo guard:** a correlation search over what was played to the speaker keeps the agent from hearing itself.
- **Tool calling:** native function calling with narration around the call, and a graceful end-of-call (`end_call`) that waits for the goodbye to be spoken.
- **Speculative replies:** the model starts answering before the caller has fully finished, and the reply is kept or dropped depending on what the caller says next (on by default, `Config.SpeculativeLLM`).
- **Provider chains with hedging:** `NewChainLLM` falls through a list of language-model providers on an error or a slow first token.
- **Context budget:** long conversations fold into a pinned summary instead of losing their beginning.
- **Instrumentation:** per-turn latency by stage, and a session meter that counts every token a conversation used, by category, for per-token billing.

## Providers

| | Included |
|---|---|
| Language model | OpenAI, Anthropic, Google, Groq, Cerebras, OpenRouter, and chains of them |
| Speech to text | Deepgram, Groq (Whisper), OpenAI (Whisper), AssemblyAI |
| Text to speech | Lokutor (Versa), streaming, with a connection pool |

Anything else is one interface away (`STTProvider`, `LLMProvider`, `TTSProvider` in `pkg/orchestrator`).

---

## Quick start

```bash
go get github.com/lokutor-ai/lokutor-orchestrator
```

```go
package main

import (
	"context"
	"os"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	llmProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/llm"
	sttProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
	ttsProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/tts"
)

func main() {
	stt := sttProvider.NewDeepgramSTT(os.Getenv("DEEPGRAM_API_KEY"))
	llm := llmProvider.NewGroqLLM(os.Getenv("GROQ_API_KEY"), "meta-llama/llama-4-scout-17b-16e-instruct")
	tts := ttsProvider.NewLokutorTTS(os.Getenv("LOKUTOR_API_KEY"))

	config := orchestrator.DefaultConfig()
	vad := orchestrator.NewImprovedRMSVAD(config.BargeInVADThreshold, 200*time.Millisecond, 44100)
	orch := orchestrator.NewWithVAD(stt, llm, tts, vad, config)

	session := orch.NewSessionWithDefaults("session_01")
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Feed 44.1 kHz 16-bit PCM from your microphone with stream.Write(chunk).
	for event := range stream.Events() {
		switch event.Type {
		case orchestrator.UserSpeaking:
			stopPlayback() // fast barge-in on your side too
		case orchestrator.AudioChunk:
			play(event.Data.([]byte))
		}
	}
}
```

To run the example agent from this repository (microphone and speakers, no code to write), put your keys in a `.env` and start it:

```env
STT_PROVIDER=deepgram        # groq | openai | deepgram | assemblyai
LLM_PROVIDER=groq            # groq | openai | anthropic | google
DEEPGRAM_API_KEY=...
GROQ_API_KEY=...
LOKUTOR_API_KEY=...          # free credit at https://app.lokutor.com
AGENT_LANGUAGE=en
```

```bash
go run ./cmd/agent
```

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
│  │ Echo Guard │──▶│ VAD      │  │
│  └────────────┘   └──────────┘  │
│          │             │        │
│          ▼             ▼        │
│  ┌────────────┐   ┌──────────┐  │
│  │ STT Stream │◀──│ Buffers  │  │
│  └────────────┘   └──────────┘  │
│          │             │        │
│          ▼             ▼        │
│  ┌────────────┐   ┌──────────┐  │
│  │ LLM Logic  │──▶│ TTS Gen  │─┐│
│  └────────────┘   └──────────┘ ││
└────────────────────────│────────┘
                         │
                         ▼
               ┌───────────────────┐
               │ Adaptive Output   │
               └───────────────────┘
```

### Latency breakdown

Every turn is instrumented and available through `stream.GetLatencyBreakdown()`:

- `User-to-STT`: from the caller stopping to the final transcript.
- `TTFB`: from the caller stopping to the first audio sample.
- `E2E`: the full turn-around to the speaker.

On Lokutor's production calls the median from the end of the caller's sentence to the first word of the reply is 596 ms (30 calls, measured 10 October 2026; [how it was measured](https://lokutor.com/benchmarks/)). Your number depends on the providers you plug in.

### Echo suppression

The orchestrator tracks every sample sent to the speaker and runs a sliding-window correlation search on the microphone input, so it can tell when the microphone is hearing the agent's own voice instead of the caller.

---

## Documentation

- [Voice Agent API](docs/VOICE_AGENT_API.md)
- [Tool Calling Guide](docs/TOOL_CALLING_GUIDE.md)
- [Changelog](docs/CHANGELOG.md) · [Contributing](docs/CONTRIBUTING.md) · [Testing](docs/TESTING.md)

## License

MIT. Built by the [Lokutor](https://lokutor.com) team in Madrid.
