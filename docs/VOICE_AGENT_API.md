# Lokutor Orchestrator: Voice Agent API Documentation

This document provides a comprehensive guide on how to use the **Lokutor Orchestrator** to build highly responsive, full-duplex voice agents. The orchestrator manages the entire pipeline from Voice Activity Detection (VAD) and Speech-to-Text (STT) to Large Language Models (LLM) and Text-to-Speech (TTS).

---

## Table of Contents
1. [Core Concepts](#core-concepts)
2. [Audio Specifications](#audio-specifications)
3. [Configuration](#configuration)
4. [Initialization](#initialization)
5. [Low-Level API](#low-level-api)
6. [Managed Stream API (Recommended)](#managed-stream-api-recommended)
7. [Event Reference](#event-reference)
8. [Session Management](#session-management)
9. [Provider Options](#provider-options)

---

## Core Concepts

The system is built around four main components defined in [pkg/orchestrator/types.go](pkg/orchestrator/types.go):

1.  **Orchestrator**: The central engine that coordinates providers.
2.  **Providers**: Interfaces for STT, LLM, TTS, and VAD.
3.  **ConversationSession**: Maintains the history (context) and settings (voice, language) for a single user interaction.
4.  **ManagedStream**: A high-level abstraction that handles real-time audio input, VAD-based segmentation, and event-driven output.

---

## Audio Specifications

By default, the engine is tuned for the following audio format:
- **Sample Rate**: 44,100 Hz (44.1kHz)
- **Channels**: 1 (Mono)
- **Format**: Signed 16-bit PCM (S16LE)
- **Bytes per Sample**: 2

These settings can be adjusted in the `orchestrator.Config` struct.

---

## Configuration

### Environment Variables
The system typically uses the following environment variables (as seen in [cmd/agent/main.go](cmd/agent/main.go)):

| Variable | Description | Example |
| :--- | :--- | :--- |
| `STT_PROVIDER` | STT provider name | `groq`, `openai`, `deepgram`, `assemblyai` |
| `LLM_PROVIDER` | LLM provider name | `groq`, `openai`, `anthropic`, `google` |
| `AGENT_LANGUAGE` | Default language code | `en`, `es`, `fr`, etc. |
| `GROQ_API_KEY` | API Key for Groq | `gsk_...` |
| `OPENAI_API_KEY` | API Key for OpenAI | `sk-...` |
| `ANTHROPIC_API_KEY`| API Key for Anthropic | `sk-ant-...` |
| `GOOGLE_API_KEY` | API Key for Google | `AIza...` |
| `DEEPGRAM_API_KEY` | API Key for Deepgram | `...` |
| `ASSEMBLYAI_API_KEY`| API Key for AssemblyAI| `...` |
| `LOKUTOR_API_KEY` | API Key for Lokutor TTS| `...` |

### Config Struct
Defined in [pkg/orchestrator/types.go](pkg/orchestrator/types.go#L140):
```go
type Config struct {
    SampleRate         int
    Channels           int
    MaxContextMessages int // Max messages to keep in history
    VoiceStyle         Voice
    Language           Language
    STTTimeout         uint
    LLMTimeout         uint
    TTSTimeout         uint
}
```

---

## Initialization

To start, you need to instantiate the providers and then the `Orchestrator`.

```go
import (
    "github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
    sttProv "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
    llmProv "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/llm"
    ttsProv "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/tts"
)

// 1. Initialize Providers
stt := sttProv.NewGroqSTT(os.Getenv("GROQ_API_KEY"), "whisper-large-v3")
llm := llmProv.NewGroqLLM(os.Getenv("GROQ_API_KEY"), "llama-3.3-70b-versatile")
tts := ttsProv.NewLokutorTTS(os.Getenv("LOKUTOR_API_KEY"))
vad := orchestrator.NewRMSVAD(0.02, 500*time.Millisecond)

// 2. Create Orchestrator
config := orchestrator.DefaultConfig()
orch := orchestrator.NewWithVAD(stt, llm, tts, vad, config)
```

---

## Low-Level API

The [Orchestrator](pkg/orchestrator/orchestrator.go) provides several modes for processing audio:

### 1. Simple Round-trip (`ProcessAudio`)
Sync processing from audio bytes to transcript and full response audio.
```go
transcript, audio, err := orch.ProcessAudio(ctx, session, inputAudio)
```

### 2. Streaming Response (`ProcessAudioStream`)
Sync input with callback-based streaming output.
```go
transcript, err := orch.ProcessAudioStream(ctx, session, inputAudio, func(chunk []byte) error {
    // Send chunk to speakers/output
    return nil
})
```

---

## Managed Stream API (Recommended)

The `ManagedStream` (defined in [pkg/orchestrator/managed_stream.go](pkg/orchestrator/managed_stream.go)) is designed for full-duplex interactions. It handles:
- **Barge-in**: Automatically interrupts the bot if the user starts talking.
- **Echo Guard**: Intelligently ignores bot audio picked up by the microphone.
- **Pre-roll**: Keeps a small buffer of audio before speech starts to avoid clipping.

### Usage Example

```go
// 1. Create a session
session := orch.NewSessionWithDefaults("user_123")

// 2. Start the stream
stream := orch.NewManagedStream(ctx, session)
defer stream.Close()

// 3. Handle Events (Run in a goroutine)
go func() {
    for event := range stream.Events() {
        switch event.Type {
        case orchestrator.AudioChunk:
            audio := event.Data.([]byte)
            // Play this audio chunk
        case orchestrator.TranscriptFinal:
            text := event.Data.(string)
            fmt.Println("User said:", text)
        case orchestrator.Interrupted:
            // Stop current playback immediately
        }
    }
}()

// 4. Write Input Audio
// Call this every time you have a new chunk from the microphone
stream.Write(micBytes)
```

---

## Event Reference

All events emitted by `ManagedStream.Events()` are of type `OrchestratorEvent`.

| Event Type | Data Type | Description |
| :--- | :--- | :--- |
| `USER_SPEAKING` | `nil` | VAD detected user has started talking. |
| `USER_STOPPED` | `nil` | User stopped talking; processing starts. |
| `TRANSCRIPT_PARTIAL`| `string` | Intermediate STT results (if supported by provider). |
| `TRANSCRIPT_FINAL` | `string` | Final transcribed text from user. |
| `BOT_THINKING` | `nil` | LLM is generating a response. |
| `BOT_SPEAKING` | `nil` | TTS has started generating audio. |
| `AUDIO_CHUNK` | `[]byte` | Raw PCM audio chunk for playback. |
| `INTERRUPTED` | `nil` | User spoke while bot was talking/thinking. |
| `ERROR` | `interface{}`| An error occurred in the pipeline. |

---

## Error Handling

The orchestrator uses custom error types defined in [pkg/orchestrator/errors.go](pkg/orchestrator/errors.go) for better error discrimination:

- `ErrEmptyTranscription`: Returned when STT results in no text (e.g., just noise).
- `ErrTranscriptionFailed`: General STT provider error.
- `ErrLLMFailed`: Problem communicating with the LLM provider.
- `ErrTTSFailed`: Problem with speech synthesis.
- `ErrNilProvider`: Initialization error when a provider is missing.

---

## Session Management

[ConversationSession](pkg/orchestrator/types.go#L160) keeps track of the dialogue state. 

- **History Limit**: Uses `MaxContextMessages` to keep the context window manageable.
- **System Prompt**: Set it via `orch.SetSystemPrompt(session, "Your prompt")`.
- **Dynamic Config**: You can change voice and language per session:
  - `session.CurrentVoice = orchestrator.VoiceM1`
  - `session.CurrentLanguage = orchestrator.LanguageEs`

---

## Web & WebSocket Integration

To use the voice agent in a web browser, you must implement a WebSocket server that wraps the orchestrator. The browser sends raw audio chunks (PCM) and receives JSON events or binary audio back.

### WebSocket Protocol Definition

- **Authentication**: Recommendation is to use a query parameter `api_key` or a `config` message.
- **Client -> Server**:
    - **JSON Config**: Initial message to set up the session.
    - **Binary Data**: Raw 16-bit PCM audio chunks (44.1kHz, Mono).
- **Server -> Client**:
    - **JSON Events**: Status updates (e.g., `USER_SPEAKING`, `TRANSCRIPT_FINAL`).
    - **Binary Data**: Response audio chunks from TTS.

### Server Implementation Example (Go)

```go
func handleAgent(w http.ResponseWriter, r *http.Request) {
    conn, err := websocket.Accept(w, r, nil)
    if err != nil { return }
    defer conn.Close(websocket.StatusInternalError, "")

    // 1. Initialize session and stream
    session := orch.NewSessionWithDefaults("web_user")
    stream := orch.NewManagedStream(r.Context(), session)

    // 2. Forward events from Orchestrator to WebSocket
    go func() {
        for event := range stream.Events() {
            if event.Type == orchestrator.AudioChunk {
                // Send binary audio
                conn.Write(r.Context(), websocket.MessageBinary, event.Data.([]byte))
            } else {
                // Send JSON event
                wsjson.Write(r.Context(), conn, event)
            }
        }
    }()

    // 3. Forward audio from WebSocket to Orchestrator
    for {
        mt, data, err := conn.Read(r.Context())
        if err != nil { break }
        if mt == websocket.MessageBinary {
            stream.Write(data)
        } else {
            // Handle JSON config/commands
        }
    }
}
```

### React Client Example

Your React code failed because it was missing the API key and potentially connecting to a production environment that hasn't deployed the `/agent` endpoint yet. For local development, use `ws://localhost:8080/agent`.

```javascript
const startSession = async () => {
    // 1. Setup Audio Context
    const audioContext = new AudioContext({ sampleRate: 44100 });
    
    // 2. Setup WebSocket (Add api_key if required)
    const ws = new WebSocket(`ws://localhost:8080/agent?api_key=${apiKey}`);
    
    ws.onmessage = async (event) => {
        if (typeof event.data === 'string') {
            const msg = JSON.parse(event.data);
            // NOTE: Use msg.data for the payload as per Go OrchestratorEvent struct
            if (msg.type === 'TRANSCRIPT_FINAL') {
                appendMessage('user', msg.data); 
            }
            // ... handle other types
        } else {
            // Binary audio chunk - Play it
            const arrayBuffer = await event.data.arrayBuffer();
            playChunk(arrayBuffer, audioContext);
        }
    };

    // 3. Send microphone data
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    // ... capture and send bit-depth converted PCM
};
```

---

## Provider Options

### Speech-to-Text (STT)
- **Groq**: Fast, reliable (Powered by Whisper).
- **OpenAI**: Standard Whisper API.
- **Deepgram**: Low latency, high accuracy.
- **AssemblyAI**: Feature-rich.

### Large Language Models (LLM)
- **Groq**: Ultra low-latency (Llama 3, Mixtral).
- **Anthropic**: High intelligence (Claude 3.5 Sonnet).
- **OpenAI**: Standard Models (GPT-4o).
- **Google**: Gemini models.

### Text-to-Speech (TTS)
- **Lokutor**: Optimized for voice agents with low-latency streaming support.

---

*Note: For a full implementation example, refer to the CLI agent in [cmd/agent/main.go](cmd/agent/main.go).*
