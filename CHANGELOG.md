# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Repository Migration Notice

**As of v1.2.1**, the repository has moved from:
- **Old**: `github.com/team-hashing/lokutor-orchestrator`
- **New**: `github.com/lokutor-ai/lokutor-orchestrator`

Please update your imports and `go.mod` files accordingly:
```bash
go get github.com/lokutor-ai/lokutor-orchestrator
```

## [1.3.3] - 2026-02-05

### Fixed
- **Deep Echo Suppression**: Added a `VAD confirmation` requirement (3 consecutive frames) to filter out transient echo pops and spikes.
- **Interruption Guard Reset**: The VAD now resets its state exactly when the bot begins its response, clearing any residue from "thinking" phase loopback.
- **Improved Adaptive Thresholding**: Bumped specialized Echo-Guard threshold to `0.35 RMS` for the first 1.2s of playback to handle high-latency delivery.

## [1.3.1] - 2026-02-05

### Fixed
- **Adaptive Echo Guard**: Integrated automatic VAD threshold adjustment in `ManagedStream` to prevent self-interruption in high-latency SDK/API environments.
- **Concurrent Session Isolation**: `ManagedStream` now clones the VAD provider to ensure thread-safe, isolated voice detection for multiple concurrent users.

## [1.3.0] - 2026-02-05

### Added
- **Full-Duplex Managed Stream**: New `ManagedStream` for autonomous voice orchestration.
- **Internal VAD Support**: `VADProvider` interface and default `RMSVAD` implementation.
- **Barge-in (Interruptions)**: Automatic bot response cancellation when user speech is detected.
- **Event Bus**: Structured events for conversation state tracking (`USER_SPEAKING`, `BOT_THINKING`, etc.).
- **Streaming STT Support**: `StreamingSTTProvider` interface for real-time partial transcripts.
- **Expanded Provider Suite**:
    - **LLM**: native support for **Anthropic** (Claude), **OpenAI** (GPT), and **Google** (Gemini).
    - **STT**: native support for **Deepgram** (Nova-2), **AssemblyAI**, and **OpenAI** (Whisper).
- **New Orchestrator Methods**: `PushAudio` and `NewManagedStream` for plug-and-play integration.
- **High-Quality Audio**: Built-in support for 44.1kHz 16-bit PCM across all providers.

## [1.2.1] - 2026-02-03

### Changed
- Migrated repository to new GitHub organization (lokutor-ai)
- Updated module path: `github.com/lokutor-ai/lokutor-orchestrator`
- Updated all documentation and badges
- No code changes; v1.2.1 has identical functionality to v1.2.0

## [1.2.0] - 2026-02-03

### Added
- Custom error types for better error discrimination:
  - `ErrEmptyTranscription`: Returned when transcription produces empty text
  - `ErrTranscriptionFailed`: Returned when STT provider fails
  - `ErrLLMFailed`: Returned when LLM provider fails
  - `ErrTTSFailed`: Returned when TTS provider fails
  - `ErrNilProvider`: Returned when a required provider is nil
  - `ErrContextCancelled`: Returned when operation is cancelled via context
- Structured logging interface (`Logger`) for production deployments
- `NewWithLogger()` method for creating Orchestrator with custom logger
- `NoOpLogger` default implementation for zero-overhead logging
- Support for structured logging in all pipeline operations
- Logging of operation timing and completion (sessionID, message length, audio size, etc.)

### Changed
- Orchestrator now uses structured logging instead of standard log package
- `New()` constructor now creates Orchestrator with no-op logger by default
- Error messages now include wrapped context with custom error types
- Improved observability with structured log fields (sessionID, messageLen, audioSize, duration)

### Fixed
- Better error propagation with distinct error types for each pipeline stage
- Clearer error messages with context about what failed

## [1.1.1] - 2026-02-03

### Added
- Helper methods on Orchestrator for better ergonomics:
  - `NewSessionWithDefaults()`: Create session with orchestrator's default config
  - `SetSystemPrompt()`: Convenience method to add system prompt
  - `SetVoice()`: Convenience method to change session voice
  - `SetLanguage()`: Convenience method to change session language
  - `ResetSession()`: Convenience method to clear session context

## [1.1.0] - 2026-02-03

### Added
- High-level `Conversation` API for simplified voice conversation patterns
- `Chat()` method: Send text, get voice response with TTS streaming
- `ProcessAudio()` method: Send audio, get transcript and voice response
- `TextOnly()` method: Send text, get text response (no TTS)
- Conversation configuration methods:
  - `SetVoice()` / `SetVoiceByString()`
  - `SetLanguage()` / `SetLanguageByString()`
  - `SetSystemPrompt()`
- Conversation context management:
  - `GetContext()`: Full conversation history
  - `GetLastUserMessage()`: Get last user message
  - `GetLastAssistantMessage()`: Get last assistant response
  - `ClearContext()`: Clear history but keep system prompt
  - `Reset()`: Clear everything
- Session introspection methods:
  - `GetSessionID()`
  - `GetProviders()`
  - `GetConfig()`
- Dual API design for flexibility:
  - High-level `Conversation` for common patterns
  - Low-level `Orchestrator` for advanced use cases

## [1.0.0] - 2026-02-03

### Added
- Initial release of Lokutor Voice Agent Go orchestrator
- Provider-agnostic STT, LLM, and TTS interface design
- Full conversation pipeline with session management
- Streaming audio support
- Comprehensive test suite (10 tests, 84.2% coverage)
- Complete documentation and examples

### Features
- `Orchestrator` type for managing voice pipelines
- `ConversationSession` for conversation state management
- Built-in support for context windowing
- Thread-safe configuration management
- Support for 10 voice styles (F1-F5, M1-M5)
- Support for 8 languages (en, es, fr, de, it, pt, ja, zh)

[1.0.0]: https://github.com/lokutor-ai/lokutor-orchestrator/releases/tag/v1.0.0
