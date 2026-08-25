package orchestrator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// TurnCompletionAnalyzer
// ---------------------------------------------------------------------------

func TestTurnCompletion_CompleteSentences(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	complete := []string{
		"What time is it?",
		"That's amazing!",
		"I want to book a table for two.",
		"My name is Dana and I called yesterday.",
		"Yeah",
		"Okay",
		"Sure thing.",
		"yes",
	}
	for _, tc := range complete {
		assert.True(t, tca.IsLikelyComplete(tc), "%q should be complete", tc)
	}
}

func TestTurnCompletion_IncompleteEndings(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	incomplete := []string{
		"I was thinking about and",
		"Let me tell you why but",
		"The reason is because",
		"I wanted to, you know",
		"So anyway I mean",
		"We could go there or",
		"What I meant was which",
		"Call me when",
		"If",
		"I think so",
	}
	for _, tc := range incomplete {
		assert.False(t, tca.IsLikelyComplete(tc), "%q should be incomplete", tc)
	}
}

func TestTurnCompletion_TrailingPunctuationPatterns(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	assert.False(t, tca.IsLikelyComplete("I ordered pizza,"), "trailing comma = mid-thought")
	assert.False(t, tca.IsLikelyComplete("I was thinking..."), "trailing ellipsis = mid-thought")
}

func TestTurnCompletion_SpanishEndings(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	assert.False(t, tca.IsLikelyComplete("Quería preguntarte porque"), "porque = incomplete")
	assert.False(t, tca.IsLikelyComplete("Estaba pensando y"), "y = incomplete")
	assert.False(t, tca.IsLikelyComplete("Llámame cuando"), "cuando = incomplete")
	assert.True(t, tca.IsLikelyComplete("Sí claro"), "claro = complete")
	assert.True(t, tca.IsLikelyComplete("Vale"), "vale = complete")
}

func TestTurnCompletion_ShortUtterances(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	// 1-2 word affirmations count as complete turns.
	assert.True(t, tca.IsLikelyComplete("yes"))
	assert.True(t, tca.IsLikelyComplete("nope"))
	assert.True(t, tca.IsLikelyComplete("sí"))

	// Short non-affirmations are ambiguous -> not complete.
	assert.False(t, tca.IsLikelyComplete("hmm"))
}

func TestTurnCompletion_ArticleEndingIncomplete(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	assert.False(t, tca.IsLikelyComplete("I would like the"))
	assert.False(t, tca.IsLikelyComplete("She went to the store to buy a"))
	assert.False(t, tca.IsLikelyComplete("Quiero el"))
}

func TestTurnCompletion_EmptyInput(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()
	assert.False(t, tca.IsLikelyComplete(""))
	assert.False(t, tca.IsLikelyComplete("   "))
}

func TestTurnCompletion_CombinedScoreTemporalWeighting(t *testing.T) {
	tca := NewTurnCompletionAnalyzer()

	// Long utterance with complete sentence => high score.
	long := tca.CombinedCompletionScore("I would like to book a table for tonight please.", 3500)
	// Same text but very short duration => lower score.
	shortDur := tca.CombinedCompletionScore("I would like to book a table for tonight please.", 200)

	assert.Greater(t, long, shortDur, "longer duration raises completion confidence")

	// Incomplete semantics cap the score even with long duration.
	bad := tca.CombinedCompletionScore("and then we", 4000)
	assert.Less(t, bad, long, "incomplete endings score below complete ones")
}

// ---------------------------------------------------------------------------
// ResponseCache
// ---------------------------------------------------------------------------

func TestResponseCache_SetGetRoundTrip(t *testing.T) {
	c := NewResponseCache(time.Minute, 10)
	key := CacheKeyFor("hello", "")

	c.Set(key, "Hi there!", []byte{1, 2, 3}, time.Minute)

	resp, audio, ok := c.Get(key)
	assert.True(t, ok)
	assert.Equal(t, "Hi there!", resp)
	assert.Equal(t, []byte{1, 2, 3}, audio)
}

func TestResponseCache_MissAndExpiry(t *testing.T) {
	c := NewResponseCache(time.Minute, 10)

	_, _, ok := c.Get(CacheKeyFor("nothing", ""))
	assert.False(t, ok, "unknown key misses")

	// TTL expiry.
	c.Set(CacheKeyFor("temp", ""), "x", nil, 20*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	_, _, ok = c.Get(CacheKeyFor("temp", ""))
	assert.False(t, ok, "expired entry must miss")
}

func TestResponseCache_Invalidate(t *testing.T) {
	c := NewResponseCache(time.Minute, 10)
	k1 := CacheKeyFor("a", "")
	k2 := CacheKeyFor("b", "")
	c.Set(k1, "A", nil, time.Minute)
	c.Set(k2, "B", nil, time.Minute)

	c.Invalidate(k1)
	_, _, ok := c.Get(k1)
	assert.False(t, ok)

	_, _, ok = c.Get(k2)
	assert.True(t, ok, "other entries unaffected")

	c.InvalidateAll()
	_, _, ok = c.Get(k2)
	assert.False(t, ok)
}

func TestResponseCache_MaxSizeEviction(t *testing.T) {
	c := NewResponseCache(time.Minute, 3)

	keys := []string{"k1", "k2", "k3", "k4"}
	for i, k := range keys {
		c.Set(CacheKeyFor(k, ""), string(rune('a'+i)), nil, time.Minute)
	}

	present := 0
	for _, k := range keys {
		if _, _, ok := c.Get(CacheKeyFor(k, "")); ok {
			present++
		}
	}
	assert.LessOrEqual(t, present, 3, "cache never exceeds maxSize")
}

func TestCacheKeyFor_Format(t *testing.T) {
	k := CacheKeyFor("what's the weather", "previous question")
	assert.Equal(t, "q:what's the weather|last:previous question", k)

	// Distinct inputs -> distinct keys.
	assert.NotEqual(t, CacheKeyFor("a", "b"), CacheKeyFor("b", "a"))
}
