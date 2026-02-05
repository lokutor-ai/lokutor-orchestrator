# Lokutor Orchestrator

[![Go Reference](https://pkg.go.dev/badge/github.com/lokutor-ai/lokutor-orchestrator.svg)](https://pkg.go.dev/github.com/lokutor-ai/lokutor-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Tests](https://img.shields.io/badge/Tests-14%2F14-brightgreen)](./TESTING.md)
[![Go Report Card](https://goreportcard.com/badge/github.com/lokutor-ai/lokutor-orchestrator)](https://goreportcard.com/report/github.com/lokutor-ai/lokutor-orchestrator)

A production-ready Go library for building voice-powered applications with pluggable providers for STT (Speech-to-Text), LLM (Language Model), and TTS (Text-to-Speech).

## Features

- ✅ **Full-Duplex Voice Orchestration (v1.3)** - Real-time capture and playback with built-in VAD
- ✅ **Barge-in Support** - Instantly interrupts the bot when the user starts speaking
- ✅ **High-Quality Audio** - Native 44.1kHz 16-bit PCM support for crystal clear voice
- ✅ **Provider-agnostic architecture** - Swap STT, LLM, and TTS implementations
- ✅ **Multiple Providers Out-of-the-box**:
    - **LLM**: Groq (Llama), OpenAI (GPT), Anthropic (Claude), Google (Gemini)
    - **STT**: Groq (Whisper), OpenAI (Whisper), Deepgram (Nova-2), AssemblyAI
    - **TTS**: Lokutor (Versa)
- ✅ **Session management** - Automatic context windowing and multi-language support
- ✅ **Event-driven API** - Thread-safe channel-based event bus for building robust UIs
- ✅ **Low Latency** - Designed for real-time voice interactions

## Installation

```bash
go get github.com/lokutor-ai/lokutor-orchestrator
```

## Quick Start

### 1. Full-Duplex Voice Agent (The v1.3 way)

This is the recommended way to build a real-time voice assistant with barge-in support.

```go
package main

import (
	"context"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	sttProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
	llmProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/llm"
	ttsProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/tts"
)

func main() {
	// Initialize Providers
	stt := sttProvider.NewGroqSTT("YOUR_GROQ_KEY", "whisper-large-v3")
	llm := llmProvider.NewGroqLLM("YOUR_GROQ_KEY", "llama-3.3-70b-versatile")
	tts := ttsProvider.NewLokutorTTS("YOUR_LOKUTOR_KEY")
	
	// Initialize VAD (Voice Activity Detection)
	vad := orchestrator.NewRMSVAD(0.02, 500*time.Millisecond)

	// Create Orchestrator
	orch := orchestrator.NewWithVAD(stt, llm, tts, vad, orchestrator.DefaultConfig())
	session := orch.NewSessionWithDefaults("user_123")

	// Start a Managed Stream
	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Handle Events
	go func() {
		for event := range stream.Events() {
			switch event.Type {
			case orchestrator.UserSpeaking:
				// Stop your audio playback here
			case orchestrator.TranscriptFinal:
				fmt.Printf("User: %s\n", event.Data.(string))
			case orchestrator.AudioChunk:
				// Play the audio chunk raw bytes
				playAudio(event.Data.([]byte))
			}
		}
	}()

	// Pipe your microphone audio bytes here
	stream.Write(micBytes)
}
```

### 2. Conversational API (Turn-based)

### Processing Audio Input

```go
// Process audio (handles STT -> LLM -> TTS)
transcript, response, err := conv.ProcessAudio(
	context.Background(),
	audioBytes,
	func(chunk []byte) error {
		// Handle audio chunk
		return sendToSpeaker(chunk)
	},
)

if err != nil {
	log.Fatal(err)
}

log.Printf("User said: %s", transcript)
log.Printf("Assistant responded: %s", response)
```

### Low-Level Orchestrator API

For advanced use cases where you need fine-grained control, use the `Orchestrator` directly:

```go
import "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"

// Create orchestrator directly
orch := orchestrator.New(stt, llm, tts, orchestrator.DefaultConfig())

// Create and manage sessions manually
session := orchestrator.NewConversationSession("user_123")
```

## Advanced Usage

### Working with Multiple Languages

The orchestrator supports seamless language switching:

```go
// Set language on a session
orch.SetLanguage(session, orchestrator.LanguageEs)

// Subsequent interacts in a ManagedStream or Conversation will use Spanish
// for both STT transcription and TTS synthesis.
```

## Advanced: Full-Duplex v1.3 (Streaming & Barge-in)

The v1.3 orchestrator supports **Full-Duplex** mode with internal **VAD (Voice Activity Detection)** and **Barge-in** (automatic interruption when the user speaks while the bot is responding).

### Features
- **Internal VAD:** Automatically detects when the user starts and stops speaking.
- **Barge-in:** Immediately cancels LLM generation and clears TTS audio buffers when user interruption is detected.
- **Event Bus:** Emits events for state changes (`UserSpeaking`, `BotThinking`, `Interrupted`, etc.).

### How to test the example agent

1.  **Clone and install dependencies:**
    ```bash
    go get ./...
    ```

2.  **Configure environment:** Create a `.env` file in the root:
    ```env
    # Provider Selection
    STT_PROVIDER=groq|openai|deepgram|assemblyai
    LLM_PROVIDER=groq|openai|anthropic|google

    # API Keys
    GROQ_API_KEY=your_groq_key
    OPENAI_API_KEY=your_openai_key
    ANTHROPIC_API_KEY=your_anthropic_key
    GOOGLE_API_KEY=your_google_key
    DEEPGRAM_API_KEY=your_deepgram_key
    ASSEMBLYAI_API_KEY=your_assemblyai_key
    LOKUTOR_API_KEY=your_lokutor_key

    # Settings
    AGENT_LANGUAGE=es # or en, fr, de, etc.
    ```

3.  **Run the agent:**
    ```bash
    go run cmd/agent/main.go
    ```

### Using ManagedStream in your app

```go
// 1. Setup providers
stt := providers.NewGroqSTT(os.Getenv("GROQ_API_KEY"), "whisper-large-v3")
llm := providers.NewGroqLLM(os.Getenv("GROQ_API_KEY"), "llama-3.3-70b-versatile")
tts := providers.NewLokutorTTS(os.Getenv("LOKUTOR_API_KEY"))
// Increased base threshold slightly for production SDK environments
vad := orchestrator.NewRMSVAD(0.04, 600*time.Millisecond) 

// 2. Initialize Orchestrator with VAD
orch := orchestrator.NewWithVAD(stt, llm, tts, vad, orchestrator.DefaultConfig())

// 3. Create a Managed Stream
ctx := context.Background()
stream := orch.NewManagedStream(ctx, session)

// 4. Feed audio in and listen for events
go func() {
    for event := range stream.Events() {
        if event.Type == orchestrator.AudioChunk {
            playAudio(event.Data.([]byte))
        }
    }
}()

// Pipe microphone audio to stream
stream.Write(microphoneBuffer)
```

// Process audio
transcript, audioBytes, err := orch.ProcessAudio(
	context.Background(),
	session,
	rawAudioData,
)

// Stream TTS output
err = orch.SynthesizeStream(
	context.Background(),
	"Hello world",
	orchestrator.VoiceF2,
	func(chunk []byte) error {
		return sendToWebSocket(chunk)
	},
)
```

## Conversation API Reference

The `Conversation` type provides a high-level, user-friendly API:

### Creating Conversations

```go
// Create with default configuration
conv := orchestrator.NewConversation(stt, llm, tts)

// Create with custom configuration
config := orchestrator.DefaultConfig()
config.VoiceStyle = orchestrator.VoiceM3
config.MaxContextMessages = 50
conv := orchestrator.NewConversationWithConfig(stt, llm, tts, config)
```

### Configuring Conversation Settings

```go
// Set voice (F1-F5, M1-M5)
conv.SetVoice(orchestrator.VoiceM2)
// or
conv.SetVoiceByString("M2")

// Set language (en, es, fr, de, it, pt, ja, zh)
conv.SetLanguage(orchestrator.LanguageEn)
// or
conv.SetLanguageByString("es")

// Set system prompt to guide LLM behavior
conv.SetSystemPrompt("You are a helpful customer service agent. Be concise and professional.")
```

### Conversation Modes

```go
// Text + Voice: Send text, get voice response with TTS streaming
response, err := conv.Chat(ctx, "What's the capital of France?", func(chunk []byte) error {
	return streamToSpeaker(chunk)
})

// Audio: Send audio, get text and voice response
transcript, response, err := conv.ProcessAudio(ctx, audioBytes, func(chunk []byte) error {
	return streamToSpeaker(chunk)
})

// Text only: No TTS, just get text response (useful for debugging)
response, err := conv.TextOnly(ctx, "What's 2+2?")
```

### Managing Conversation Context

```go
// Get full conversation history
messages := conv.GetContext()

// Get specific messages
lastUser := conv.GetLastUserMessage()
lastAssistant := conv.GetLastAssistantMessage()

// Clear conversation but keep system prompt
conv.ClearContext()

// Reset everything to fresh state
conv.Reset()
```

### Introspection

```go
// Get session ID
sessionID := conv.GetSessionID()

// Get provider information
providers := conv.GetProviders() // {"stt": "...", "llm": "...", "tts": "..."}

config := config.GetConfig()
```

## Error Handling

The library uses custom error types to allow precise error handling:

```go
import "errors"

transcript, response, err := conv.ProcessAudio(ctx, audioBytes, onAudioChunk)
if err != nil {
	// Check for specific error types
	if errors.Is(err, orchestrator.ErrEmptyTranscription) {
		// Handle empty transcription (user didn't speak)
		return "User said nothing, please try again"
	}
	
	if errors.Is(err, orchestrator.ErrLLMFailed) {
		// Handle LLM failures (API down, rate limited, etc.)
		return "Unable to generate response, please try again"
	}
	
	if errors.Is(err, orchestrator.ErrTTSFailed) {
		// Handle TTS failures (voice synthesis failed)
		return "Audio synthesis failed"
	}
	
	// Generic error handling
	log.Fatalf("Unexpected error: %v", err)
}
```

### Available Error Types

- `ErrEmptyTranscription`: Transcription produced empty text
- `ErrTranscriptionFailed`: STT provider failed
- `ErrLLMFailed`: LLM provider failed
- `ErrTTSFailed`: TTS provider failed
- `ErrNilProvider`: Required provider is nil
- `ErrContextCancelled`: Operation cancelled by context

## Structured Logging

For production deployments, provide a custom logger for observability and monitoring:

```go
import "log"

// Example: Simple structured logger
type SimpleLogger struct{}

func (l *SimpleLogger) Debug(msg string, args ...interface{}) {
	log.Printf("[DEBUG] %s %v", msg, args)
}

func (l *SimpleLogger) Info(msg string, args ...interface{}) {
	log.Printf("[INFO] %s %v", msg, args)
}

func (l *SimpleLogger) Warn(msg string, args ...interface{}) {
	log.Printf("[WARN] %s %v", msg, args)
}

func (l *SimpleLogger) Error(msg string, args ...interface{}) {
	log.Printf("[ERROR] %s %v", msg, args)
}

// Create orchestrator with custom logger
logger := &SimpleLogger{}
orch := orchestrator.NewWithLogger(stt, llm, tts, config, logger)
```

### Default Behavior

If no logger is provided, a no-op logger is used by default (zero overhead):

```go
// This uses the default no-op logger
orch := orchestrator.New(stt, llm, tts, config)
```

### Logger Interface

The `Logger` interface allows integration with any logging framework:

```go
type Logger interface {
	Debug(msg string, args ...interface{})
	Info(msg string, args ...interface{})
	Warn(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}
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
	SampleRate:         44100,
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
	SampleRate:         44100,
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
