package orchestrator

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSession() *ConversationSession {
	return NewConversationSession("user-test-123")
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

func TestSession_Defaults(t *testing.T) {
	s := newTestSession()
	assert.Equal(t, "user-test-123", s.ID)
	assert.Empty(t, s.Context)
	assert.Equal(t, 20, s.MaxMessages)
	assert.Equal(t, VoiceF1, s.GetCurrentVoice())
	assert.Equal(t, LanguageEn, s.CurrentLanguage)
	assert.Empty(t, s.UserMemory)
}

// ---------------------------------------------------------------------------
// Message history & trimming
// ---------------------------------------------------------------------------

func TestSession_AddMessageTrimsToMax(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 5

	for i := 0; i < 12; i++ {
		s.AddMessage("user", "msg")
	}
	assert.LessOrEqual(t, len(s.Context), 5, "context must never exceed MaxMessages")

	// The newest messages survive the trim.
	last := s.GetContextCopy()
	assert.Equal(t, "msg", last[len(last)-1].Content)
}

func TestSession_LastUserAndAssistantTracking(t *testing.T) {
	s := newTestSession()
	s.AddMessage("user", "hello")
	s.AddMessage("assistant", "hi there")
	assert.Equal(t, "hello", s.LastUser)
	assert.Equal(t, "hi there", s.LastAssistant)

	s.AddMessage("user", "how are you")
	assert.Equal(t, "how are you", s.LastUser)
	assert.Equal(t, "hi there", s.LastAssistant, "LastAssistant unchanged by user message")

	// Empty assistant messages do NOT overwrite LastAssistant.
	s.AddMessage("assistant", "")
	assert.Equal(t, "hi there", s.LastAssistant)
}

func TestSession_UpdateLastUserMessage(t *testing.T) {
	s := newTestSession()
	s.AddMessage("user", "first draft")
	s.AddMessage("assistant", "reply")

	s.UpdateLastUserMessage("corrected text")
	assert.Equal(t, "corrected text", s.LastUser)

	ctx := s.GetContextCopy()
	found := false
	for _, m := range ctx {
		if m.Role == "user" && m.Content == "corrected text" {
			found = true
		}
	}
	assert.True(t, found, "context updated in place")

	// No prior user message -> appended as fallback.
	s2 := newTestSession()
	s2.UpdateLastUserMessage("orphan")
	require.Len(t, s2.Context, 1)
	assert.Equal(t, "orphan", s2.LastUser)
}

func TestSession_ClearContext(t *testing.T) {
	s := newTestSession()
	s.AddMessage("user", "x")
	s.AddMessage("assistant", "y")

	s.ClearContext()
	assert.Empty(t, s.Context)
	assert.Empty(t, s.LastUser)
	assert.Empty(t, s.LastAssistant)
}

// ---------------------------------------------------------------------------
// Context summarization
// ---------------------------------------------------------------------------

func TestSession_NeedsSummarization(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 10

	assert.False(t, s.NeedsSummarization())
	for i := 0; i < 10; i++ {
		s.AddMessage("user", "m")
	}
	assert.False(t, s.NeedsSummarization(), "AddMessage trims AT max, never over")

	// Simulate runtime policy change (e.g. memory pressure lowering the cap).
	s.MaxMessages = 4
	assert.True(t, s.NeedsSummarization(), "context over reduced cap triggers summarization")
}

func TestSession_SummarizeContext_ReplacesOldWithSummary(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 20

	for i := 0; i < 12; i++ {
		s.AddMessage("user", "old msg")
	}
	// Lower cap to force summarization territory.
	s.MaxMessages = 6
	require.True(t, s.NeedsSummarization())

	s.SummarizeContext("the user said many things", 2)

	ctx := s.GetContextCopy()
	assert.LessOrEqual(t, len(ctx), s.MaxMessages+1, "post-summary context within bounds (+1 summary msg)")

	foundSummary := false
	for _, m := range ctx {
		if m.Role == "system" && contains(m.Content, "[Summary of earlier conversation:") {
			foundSummary = true
		}
	}
	assert.True(t, foundSummary, "summary system message present after trim")
}

func TestSession_SummarizeContext_NoOpWhenUnderLimit(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 10
	for i := 0; i < 3; i++ {
		s.AddMessage("user", "keep me")
	}
	before := s.GetContextCopy()

	s.SummarizeContext("ignored", 2)

	assert.Equal(t, before, s.GetContextCopy(), "under-limit summarize is a no-op")
}

func TestSession_SummarizeContext_EmptySummarySkipsInjection(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 2
	for i := 0; i < 6; i++ {
		s.AddMessage("user", "old")
	}

	s.SummarizeContext("", 1)
	ctx := s.GetContextCopy()
	for _, m := range ctx {
		assert.NotEqual(t, "system", m.Role, "no summary injected when summaryText empty")
	}
}

// ---------------------------------------------------------------------------
// Defensive copies & getters
// ---------------------------------------------------------------------------

func TestSession_GetContextCopyIsDefensive(t *testing.T) {
	s := newTestSession()
	s.AddMessage("user", "original")

	cp := s.GetContextCopy()
	cp[0].Content = "MUTATED"
	cp = append(cp, Message{Role: "hacker"})

	assert.Equal(t, "original", s.Context[0].Content, "internal context unaffected by copy mutation")
	assert.Len(t, s.Context, 1, "append to copy must not leak into internal slice")
}

func TestSession_GetCurrentLanguageNormalization(t *testing.T) {
	s := newTestSession()

	s.CurrentLanguage = LanguageEn
	assert.Equal(t, LanguageEn, s.GetCurrentLanguage())

	// Agnostic modes normalize to "" so STT auto-detects.
	s.CurrentLanguage = "na"
	assert.Equal(t, Language(""), s.GetCurrentLanguage())
	s.CurrentLanguage = "auto"
	assert.Equal(t, Language(""), s.GetCurrentLanguage())
}

// ---------------------------------------------------------------------------
// Tool call loop protection
// ---------------------------------------------------------------------------

func TestSession_RecordToolCallLimit(t *testing.T) {
	s := newTestSession()

	assert.True(t, s.RecordToolCall("get_weather"), "call 1 allowed")
	assert.True(t, s.RecordToolCall("get_weather"), "call 2 allowed")
	assert.True(t, s.RecordToolCall("get_weather"), "call 3 allowed")
	assert.False(t, s.RecordToolCall("get_weather"), "call 4 blocked: infinite-loop guard")
	assert.False(t, s.RecordToolCall("get_weather"), "still blocked")

	// Independent counters per tool.
	assert.True(t, s.RecordToolCall("other_tool"), "different tool has its own budget")
}

func TestSession_ResetToolCallCounts(t *testing.T) {
	s := newTestSession()

	for i := 0; i < 5; i++ {
		s.RecordToolCall("loop_tool")
	}
	assert.False(t, s.RecordToolCall("loop_tool"))

	s.ResetToolCallCounts()
	assert.True(t, s.RecordToolCall("loop_tool"), "reset restores full budget")
}

// ---------------------------------------------------------------------------
// Tools set/get
// ---------------------------------------------------------------------------

func TestSession_SetGetTools(t *testing.T) {
	s := newTestSession()
	assert.Empty(t, s.GetTools())

	tools := []Tool{{Type: "function"}, {Type: "function"}}
	s.SetTools(tools)

	got := s.GetTools()
	require.Len(t, got, 2)
	assert.Equal(t, "function", got[0].Type)
}

// ---------------------------------------------------------------------------
// UserMemory (cross-call persistence hook)
// ---------------------------------------------------------------------------

func TestSession_UserMemoryField(t *testing.T) {
	s := newTestSession()
	assert.Empty(t, s.UserMemory)

	s.UserMemory = "User's name is Dana; prefers Spanish."
	assert.Contains(t, s.UserMemory, "Dana")
}

// ---------------------------------------------------------------------------
// Concurrency safety (run under -race in CI)
// ---------------------------------------------------------------------------

func TestSession_ConcurrentAccess(t *testing.T) {
	s := newTestSession()
	s.MaxMessages = 100

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.AddMessage("user", "concurrent")
				s.AddMessage("assistant", "reply")
				_ = s.GetContextCopy()
				_ = s.NeedsSummarization()
				s.RecordToolCall("tool")
			}
		}(g)
	}
	wg.Wait()

	assert.LessOrEqual(t, len(s.Context), 100)
}

// contains is a tiny helper to avoid importing strings everywhere.
func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
