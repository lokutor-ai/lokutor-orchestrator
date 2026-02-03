# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[1.0.0]: https://github.com/lokutorTTS/go-voice-agent/releases/tag/v1.0.0
