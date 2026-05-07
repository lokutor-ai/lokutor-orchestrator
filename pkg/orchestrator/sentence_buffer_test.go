package orchestrator

import "testing"

func TestSentenceBufferEmpty(t *testing.T) {
	sb := NewSentenceBuffer()
	if !sb.IsEmpty() {
		t.Error("Expected empty buffer initially")
	}
	if r := sb.Feed(""); r != "" {
		t.Errorf("Expected empty string from empty feed, got %q", r)
	}
}

func TestSentenceBufferFirstSentenceAtBoundary(t *testing.T) {
	sb := NewSentenceBuffer()
	// Feed tokens one by one like an LLM stream
	// The sentence should be emitted when the "?" creates a boundary at minTokens
	tokens := []string{"Hello", " ", "how", " ", "are", " ", "you", "?"}
	emitted := ""
	for _, tok := range tokens {
		if s := sb.Feed(tok); s != "" {
			emitted = s
		}
	}
	if emitted != "Hello how are you?" {
		t.Errorf("Expected 'Hello how are you?', got %q", emitted)
	}
}

func TestSentenceBufferMultipleSentences(t *testing.T) {
	sb := NewSentenceBuffer()
	var sentences []string
	flush := func(s string) {
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	// Simulate streaming a two-sentence response
	text := "I'm fine thanks. How are you?"
	for _, r := range text {
		tok := string(r)
		s := sb.Feed(tok)
		flush(s)
	}
	// Feed remaining
	flush(sb.Remaining())

	if len(sentences) != 2 {
		t.Fatalf("Expected 2 sentences, got %d: %v", len(sentences), sentences)
	}
	if sentences[0] != "I'm fine thanks." {
		t.Errorf("Expected 'I'm fine thanks.', got %q", sentences[0])
	}
	if sentences[1] != "How are you?" {
		t.Errorf("Expected 'How are you?', got %q", sentences[1])
	}
}

func TestSentenceBufferSingleSentenceNoFinalPunct(t *testing.T) {
	sb := NewSentenceBuffer()
	tokens := []string{"This", " ", "is", " ", "a", " ", "test"}
	for _, tok := range tokens {
		sb.Feed(tok)
	}
	r := sb.Remaining()
	if r != "This is a test" {
		t.Errorf("Expected 'This is a test', got %q", r)
	}
}

func TestSentenceBufferAbbreviationSkips(t *testing.T) {
	sb := NewSentenceBuffer()
	// "Dr. Smith" should NOT trigger a sentence boundary at "Dr."
	tokens := []string{"Dr.", " ", "Smith", " ", "is", " ", "here", "."}
	emitted := ""
	for _, tok := range tokens {
		s := sb.Feed(tok)
		if s == "Dr." {
			t.Error("Should not split at abbreviation 'Dr.'")
		}
		if s != "" {
			emitted = s
		}
	}
	if emitted != "Dr. Smith is here." && emitted != "" {
		t.Errorf("Expected 'Dr. Smith is here.' to be emitted, got %q", emitted)
	}
	if emitted == "" {
		r := sb.Remaining()
		if r != "Dr. Smith is here." {
			t.Errorf("Expected 'Dr. Smith is here.', got %q", r)
		}
	}
}

func TestSentenceBufferReset(t *testing.T) {
	sb := NewSentenceBuffer()
	sb.Feed("Hello")
	sb.Reset()
	if !sb.IsEmpty() {
		t.Error("Expected empty after reset")
	}
	if sb.firstSentence != true {
		t.Error("Expected firstSentence=true after reset")
	}
}

func TestSentenceBufferSpanishPunct(t *testing.T) {
	sb := NewSentenceBuffer()
	tokens := []string{"¿", "Cómo", " ", "estás", "?", " ", "Bien", "."}
	var sentences []string
	for _, tok := range tokens {
		if s := sb.Feed(tok); s != "" {
			sentences = append(sentences, s)
		}
	}
	if r := sb.Remaining(); r != "" {
		sentences = append(sentences, r)
	}
	if len(sentences) < 1 {
		t.Fatal("Expected at least one sentence")
	}
	if sentences[0] != "¿Cómo estás?" {
		t.Errorf("Expected '¿Cómo estás?', got %q", sentences[0])
	}
}

func TestSentenceBufferHardMaxFlush(t *testing.T) {
	sb := NewSentenceBuffer()
	sb.hardMaxTokens = 5
	sb.clauseTokens = 3
	sb.minTokens = 2
	// 5 multi-word tokens without sentence boundary
	tokens := []string{"hello ", "world ", "foo ", "bar ", "baz"}
	var result string
	for _, tok := range tokens {
		if s := sb.Feed(tok); s != "" {
			result = s
		}
	}
	if result == "" {
		t.Fatal("Expected non-empty flush at hardMax=5")
	}
	if result != "hello world foo bar baz" {
		t.Errorf("Expected 'hello world foo bar baz', got %q", result)
	}
}

func TestSentenceBufferMultipleBoundaries(t *testing.T) {
	sb := NewSentenceBuffer()
	text := "First! Second? Third."
	for _, r := range text {
		s := sb.Feed(string(r))
		if s != "" {
			t.Logf("Emitted: %q", s)
		}
	}
	r := sb.Remaining()
	if r == "" {
		t.Fatal("Expected remaining after multi-sentence stream")
	}
	if r != "Third." {
		t.Errorf("Expected remaining 'Third.', got %q", r)
	}
}
