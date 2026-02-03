# Lokutor Orchestrator

[![Go Reference](https://pkg.go.dev/badge/github.com/lokutorTTS/lokutor-orchestrator.svg)](https://pkg.go.dev/github.com/lokutorTTS/lokutor-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Tests](https://img.shields.io/badge/Tests-10%2F10-brightgreen)](./TESTING.md)
[![Go Report Card](https://goreportcard.com/badge/github.com/lokutorTTS/lokutor-orchestrator)](https://goreportcard.com/report/github.com/lokutorTTS/lokutor-orchestrator)

A production-ready Go library for building voice-powered applications with pluggable providers for STT (Speech-to-Text), LLM (Language Model), and TTS (Text-to-Speech).

## Features

- ✅ **Provider-agnostic architecture** - Swap STT, LLM, and TTS implementations without changing core logic
- ✅ **Full conversation pipeline** - Handles transcription, response generation, and synthesis
- ✅ **Session management** - Automatic context windowing and conversation history tracking
- ✅ **Streaming support** - Stream audio chunks from TTS in real-time
- ✅ **Zero external dependencies** - Pure Go core (adapters are optional)

## Installation

```bashlokutor-
go get github.com/lokutorTTS/orchestrator
```

## Quick Start

### Basic Usage

```go
package main

import (
	"context"
	"log"
	"github.com/lokutorTTS/lokutor-orchestrator"
)

func main() {
	// Create your providers (implement STTProvider, LLMProvider, TTSProvider)
	stt := MySTTProvider{}
	llm := MyLLMProvider{}
	tts := MyTTSProvider{}

	// Create orchestrator
	orch := orchestrator.New(stt, llm, tts, orchestrator.DefaultConfig())

	// Process audio
	session := orchestrator.NewConversationSession("user_123")
	transcript, audioBytes, err := orch.ProcessAudio(
		context.Background(),
		session,
		rawAudioData,
	)

	log.Printf("User said: %s", transcript)
	log.Printf("Agent responded with %d bytes of audio", len(audioBytes))
}
```

### Streaming TTS Output

```go
session := orchestrator.NewConversationSession("user_123")

orch.ProcessAudioStream(
	context.Background(),
	session,
	rawAudioData,
	func(chunk []byte) error {
		// Stream audio chunk to client
		return sendToWebSocket(chunk)
	},
)
```

## Creating Custom Providers

### Speech-to-Text Provider

```go
type MySTTProvider struct {}

func (p *MySTTProvider) Transcribe(ctx context.Context, audio []byte) (string, error) {
	// Your STT implementation
	return "transcribed text", nil
}

func (p *MySTTProvider) Name() string {
	return "My STT"
}
```

### Language Model Provider

```go
type MyLLMProvider struct {}

func (p *MyLLMProvider) Complete(ctx context.Context, messages []orchestrator.Message) (string, error) {
	// Your LLM implementation
	// messages contain conversation history
	return "response text", nil
}

func (p *MyLLMProvider) Name() string {
	return "My LLM"
}
```

### Text-to-Speech Provider

```go
type MyTTSProvider struct {}

func (p *MyTTSProvider) Synthesize(ctx context.Context, text string, voice orchestrator.Voice) ([]byte, error) {
	// Your TTS implementation
	return audioBytes, nil
}

func (p *MyTTSProvider) StreamSynthesize(ctx context.Context, text string, voice orchestrator.Voice, onChunk func([]byte) error) error {
	// Stream implementation
	return onChunk(audioChunk)
}

func (p *MyTTSProvider) Name() string {
	return "My TTS"
}
```

## Configuration

### DefaultConfig

```go
Config{
	SampleRate:         16000,
	Channels:           1,
	BytesPerSamp:       2,
	MaxContextMessages: 20,
	VoiceStyle:         VoiceF1,
	Language:           LanguageEn,
	STTTimeout:         30,
	LLMTimeout:         60,
	TTSTimeout:         30,
}
```

### Custom Configuration

```go
config := orchestrator.Config{
	SampleRate:         16000,
	Channels:           1,
	BytesPerSamp:       2,
	MaxContextMessages: 50,  // Keep more context
	VoiceStyle:         orchestrator.VoiceM2,
	Language:           orchestrator.LanguageEs,
	STTTimeout:         45,
	LLMTimeout:         90,
	TTSTimeout:         30,
}

orch := orchestrator.New(stt, llm, tts, config)
```

## Conversation Sessions

Sessions automatically manage conversation history and context:

```go
session := orchestrator.NewConversationSession("user_123")

// Messages are automatically added during processing
orch.ProcessAudio(ctx, session, audio)

// Access conversation history
for _, msg := range session.Context {
	log.Printf("%s: %s", msg.Role, msg.Content)
}

// Clear conversation
session.ClearContext()

// Change voice mid-conversation
session.CurrentVoice = orchestrator.VoiceM1
```

## Architecture

```
┌─────────────┐
│  Raw Audio  │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────┐
│   Orchestrator              │
│  ┌───────────┐              │
│  │    STT    │──────────────┤─► Transcript
│  └───────────┘              │
│  ┌───────────┐              │
│  │    LLM    │◄─ Context ───┤
│  └───────────┘              │
│  ┌───────────┐              │
│  │    TTS    │──────────────┤─► Audio
│  └───────────┘              │
└─────────────────────────────┘
```

## License

MIT
