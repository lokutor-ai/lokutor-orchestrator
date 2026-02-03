# Lokutor Orchestrator - Enhancement Roadmap

## Quick Summary
The library is **production-ready and professional-grade**. Below is a prioritized roadmap for enhancements to approach Pipecat's level of maturity.

---

## Phase 1: Polish (v1.2.0) - Recommended

These are quick wins that significantly improve professionalism without changing the API.

### 1.1 Add Custom Error Types ⭐ High Impact
**Why**: Allows callers to handle specific error cases
**Effort**: 30 minutes
**File**: `errors.go` (new file)

```go
package orchestrator

import "errors"

var (
    ErrEmptyTranscription = errors.New("transcription returned empty text")
    ErrNoProviders = errors.New("orchestrator missing required providers")
    ErrContextCancelled = errors.New("operation cancelled by context")
)
```

Then update `orchestrator.go` to return these instead of generic messages.

### 1.2 Add Structured Logging Interface ⭐ High Impact
**Why**: Production apps need structured logging; doesn't force a dependency
**Effort**: 1 hour
**Files**: Update `orchestrator.go`, `conversation.go`

```go
// In types.go
type Logger interface {
    Debug(msg string, args ...interface{})
    Info(msg string, args ...interface{})
    Warn(msg string, args ...interface{})
    Error(msg string, args ...interface{})
}

// Default no-op logger
type NoOpLogger struct{}
func (n *NoOpLogger) Debug(msg string, args ...interface{}) {}
// ... etc
```

Then:
```go
// In Orchestrator
func New(stt STTProvider, llm LLMProvider, tts TTSProvider, config Config, logger Logger) *Orchestrator {
    if logger == nil {
        logger = &NoOpLogger{}
    }
    return &Orchestrator{
        stt:    stt,
        llm:    llm,
        tts:    tts,
        config: config,
        logger: logger,
    }
}
```

### 1.3 Add CHANGELOG.md ⭐ Medium Impact
**Why**: Professional open source projects track changes
**Effort**: 15 minutes
**What**: Document v1.0.0, v1.1.0, v1.1.1, v1.2.0 changes

```markdown
# Changelog

All notable changes to this project will be documented in this file.

## [1.2.0] - 2026-02-04
### Added
- Custom error types for better error discrimination
- Structured logging interface (Logger)
- Performance timing in streaming operations

### Changed
- Orchestrator.New() now accepts optional Logger parameter

## [1.1.1] - 2026-02-03
### Added
- Helper methods on Orchestrator (NewSessionWithDefaults, SetSystemPrompt, etc.)

## [1.1.0] - 2026-02-03
### Added
- High-level Conversation API
- Chat, ProcessAudio, TextOnly methods

## [1.0.0] - Initial Release
```

### 1.4 Add Concurrency Tests
**Why**: Ensure session safety with concurrent operations
**Effort**: 45 minutes
**File**: Add to `orchestrator_test.go`

```go
func TestConcurrentOperations(t *testing.T) {
    // Test multiple goroutines accessing same session
}

func TestConfigThreadSafety(t *testing.T) {
    // Test concurrent config reads and writes
}
```

### 1.5 Add Context Cancellation Tests
**Why**: Verify proper context handling
**Effort**: 30 minutes

```go
func TestContextCancellation(t *testing.T) {
    // Test that cancelled contexts are respected
}
```

---

## Phase 2: Observability (v1.3.0) - Recommended for Production Use

### 2.1 Add Timing Metrics
**What**: Track how long each pipeline stage takes
**Why**: Essential for production monitoring

```go
type PipelineMetrics struct {
    STTDuration   time.Duration
    LLMDuration   time.Duration
    TTSDuration   time.Duration
    TotalDuration time.Duration
}

// Return from ProcessAudio operations
```

### 2.2 Add Callback Metrics
**What**: Report metrics back to caller
```go
type MetricsCallback func(metrics PipelineMetrics)

// Add to Conversation methods:
ProcessAudioWithMetrics(ctx, audio, onAudioChunk, onMetrics)
```

### 2.3 Provider Instrumentation
**What**: Let providers report timing/status back
```go
type Provider interface {
    // ... existing methods ...
    GetMetrics() ProviderMetrics // New
}
```

---

## Phase 3: Advanced Features (v2.0.0) - Optional

### 3.1 Interruption Handling
**What**: Gracefully handle mid-response interruptions

### 3.2 Middleware/Interceptor Pattern
**What**: Allow filtering, transforming messages

### 3.3 Provider Fallbacks
**What**: Automatically fall back to secondary providers on failure

### 3.4 Voice Activity Detection Integration
**What**: Detect when user is speaking vs pausing

---

## Immediate Action Items (This Week)

### For Immediate v1.2.0 Release

- [ ] Add custom error types (`errors.go`)
- [ ] Add structured logging interface (update `types.go`, modify `New()`)
- [ ] Write CHANGELOG.md with all version history
- [ ] Add 2-3 concurrency tests
- [ ] Add context cancellation tests
- [ ] Update README with error handling examples
- [ ] Tag v1.2.0 and push

**Estimated time**: 3-4 hours
**Impact**: Brings library to 92/100 professionalism

---

## Documentation Additions (Quick Wins)

### Add to README

**Section: Error Handling**
```markdown
## Error Handling

### Custom Error Types
The library uses custom error types for specific failures:

```go
if errors.Is(err, orchestrator.ErrEmptyTranscription) {
    // Handle empty audio case
}
```
```

**Section: Logging**
```markdown
## Logging

### Structured Logging

Provide a logger to the Orchestrator for production environments:

```go
myLogger := &MyStructuredLogger{}
orch := orchestrator.New(stt, llm, tts, config, myLogger)
```
```

**Section: Production Deployment Checklist**
```markdown
- Use structured logger (not default)
- Set appropriate timeouts in Config
- Monitor metrics from callbacks
- Test error scenarios
- Configure providers with retry logic
```

---

## Summary: What to Do NOW

**✅ DO NOW (High Impact, Low Effort)**
1. Add custom error types (30 min)
2. Add logging interface (1 hour) 
3. Create CHANGELOG.md (15 min)
4. Add 2-3 concurrency tests (45 min)
5. Update README with examples (30 min)
6. Tag v1.2.0

**Total Time**: ~3.5 hours
**Result**: Library goes from 85/100 → 92/100

**✅ LATER (Medium Priority)**
1. Add timing metrics
2. Add more comprehensive tests
3. Add real provider examples

---

## Questions to Consider

1. **Logging**: Should logging be optional (current direction) or always available?
2. **Error Types**: How granular should error types be? (Current plan: 3-5 main errors)
3. **Metrics**: Should metrics be returned or pushed to a callback?
4. **Provider Interface**: Should providers have optional methods (logging, metrics)?

**Current Recommendation**: Keep it optional and non-invasive. Provide clear extension points but don't force them.
