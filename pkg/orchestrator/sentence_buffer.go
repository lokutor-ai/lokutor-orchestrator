package orchestrator

import (
	"fmt"
	"strings"
	"unicode"
)

type SentenceBuffer struct {
	buf           strings.Builder
	tokenCount    int
	firstSentence bool
	minTokens     int
	hardMaxTokens int
	clauseTokens  int
	lastEmitted   string
}

func NewSentenceBuffer() *SentenceBuffer {
	return &SentenceBuffer{
		firstSentence: true,
		minTokens:     1,
		hardMaxTokens: 96,
		clauseTokens:  24,
	}
}

var abbreviations = map[string]bool{
	"dr": true, "mr": true, "mrs": true, "ms": true, "sra": true,
	"prof": true, "phd": true, "md": true, "jr": true, "ii": true, "iii": true,
	"etc": true, "vs": true, "inc": true, "ltd": true, "co": true, "corp": true,
	"dept": true, "est": true, "govt": true, "st": true, "ave": true, "blvd": true,
	"e": true, "w": true, "n": true, "s": true, "ne": true, "nw": true, "se": true, "sw": true,
	"jan": true, "feb": true, "mar": true, "apr": true, "jun": true, "jul": true,
	"aug": true, "sep": true, "oct": true, "nov": true, "dec": true,
	"u": true,
}

func isAbbreviation(segment string) bool {
	clean := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(segment, "."), "!"), "?")
	clean = strings.ToLower(strings.TrimSpace(clean))
	return abbreviations[clean]
}

var sentenceFeedCount int

func (sb *SentenceBuffer) Feed(token string) string {
	sb.buf.WriteString(token)
	sb.tokenCount++

	full := sb.buf.String()

	if sb.firstSentence && sb.tokenCount >= sb.minTokens {
		if s := sb.extractAtBoundary(full); s != "" {
			if s == sb.lastEmitted {
				remaining := sb.extractRemaining(full, s)
				sb.buf.Reset()
				sb.buf.WriteString(remaining)
				sb.tokenCount = countWords(remaining)
				return ""
			}
			sb.lastEmitted = s
			remaining := sb.extractRemaining(full, s)
			sb.buf.Reset()
			sb.buf.WriteString(remaining)
			sb.tokenCount = countWords(remaining)
			sb.firstSentence = false
			sb.minTokens = 8
			return s
		}
	}

	if sb.tokenCount >= sb.hardMaxTokens {
		return sb.flush()
	}

	if sb.tokenCount >= sb.clauseTokens {
		if s := sb.extractAtBoundary(full); s != "" {
			if s == sb.lastEmitted {
				remaining := sb.extractRemaining(full, s)
				sb.buf.Reset()
				sb.buf.WriteString(remaining)
				sb.tokenCount = countWords(remaining)
				return ""
			}
			sb.lastEmitted = s
			remaining := sb.extractRemaining(full, s)
			sb.buf.Reset()
			sb.buf.WriteString(remaining)
			sb.tokenCount = countWords(remaining)
			sb.firstSentence = false
			return s
		}
	}

	return ""
}

func (sb *SentenceBuffer) extractAtBoundary(text string) string {
	runes := []rune(text)
	lastEnd := -1
	depth := 0

	for i, r := range runes {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}

		if depth > 0 {
			continue
		}

		if r == '.' || r == '!' || r == '?' || r == '¿' || r == '¡' {
			if i == 0 {
				continue
			}
			before := string(runes[:i])
			lastWord := extractLastWord(before)
			if isAbbreviation(lastWord) {
				continue
			}
			if r == '.' && i+1 < len(runes) {
				next := runes[i+1]
				if unicode.IsDigit(next) {
					continue
				}
			}
			lastEnd = i + 1
		}
	}

	if lastEnd > 0 {
		candidate := strings.TrimSpace(string(runes[:lastEnd]))
		words := strings.Fields(candidate)
		if len(words) >= 2 {
			return candidate
		}
	}

	return ""
}

func (sb *SentenceBuffer) extractRemaining(full string, s string) string {
	sTrimmed := strings.TrimSpace(s)
	fullTrimmed := strings.TrimSpace(full)
	if strings.HasPrefix(fullTrimmed, sTrimmed) {
		return strings.TrimPrefix(fullTrimmed, sTrimmed)
	}
	return ""
}

func (sb *SentenceBuffer) flush() string {
	text := strings.TrimSpace(sb.buf.String())
	sb.buf.Reset()
	sb.tokenCount = 0
	sb.firstSentence = false
	sb.minTokens = 8
	if len(strings.Fields(text)) >= 2 {
		return text
	}
	return ""
}

func (sb *SentenceBuffer) Remaining() string {
	return strings.TrimSpace(sb.buf.String())
}

func (sb *SentenceBuffer) extractFirstSentence(text string) string {
	runes := []rune(text)
	depth := 0

	for i, r := range runes {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth > 0 {
			continue
		}
		if r == '.' || r == '!' || r == '?' || r == '¿' || r == '¡' {
			if i == 0 {
				continue
			}
			before := string(runes[:i])
			lastWord := extractLastWord(before)
			if isAbbreviation(lastWord) {
				continue
			}
			if r == '.' && i+1 < len(runes) {
				next := runes[i+1]
				if unicode.IsDigit(next) {
					continue
				}
			}
			candidate := strings.TrimSpace(string(runes[:i+1]))
			if len(strings.Fields(candidate)) >= 2 {
				return candidate
			}
		}
	}
	return ""
}

// DrainRemaining clears the buffer and returns all remaining content split into proper sentences
func (sb *SentenceBuffer) DrainRemaining() []string {
	var sentences []string
	text := strings.TrimSpace(sb.buf.String())
	rawLen := sb.buf.Len()
	sb.buf.Reset()
	sb.tokenCount = 0
	sb.firstSentence = true
	sb.minTokens = 4
	sb.lastEmitted = ""

	wordCount := len(strings.Fields(text))
	fmt.Printf("\r\033[K🔍 [DRAIN-BUF] rawLen=%d wordCount=%d text=%q\n", rawLen, wordCount, text[:minInt(80, len(text))])

	if wordCount < 2 {
		fmt.Printf("\r\033[K🔍 [DRAIN-BUF] too few words, skipping\n")
		return nil
	}

	// Extract sentences one at a time using first-boundary detection
	for {
		s := sb.extractFirstSentence(text)
		if s == "" {
			fmt.Printf("\r\033[K🔍 [DRAIN-EX] extractFirstSentence returned empty for %q\n", text[:minInt(60, len(text))])
			break
		}
		fmt.Printf("\r\033[K🔍 [DRAIN-EX] extracted: %q\n", s[:minInt(50, len(s))])
		sentences = append(sentences, s)
		text = sb.extractRemaining(text, s)
		if text == "" {
			break
		}
	}

	// No final "remaining" append needed - text is already empty if fully drained

	return sentences
}

func (sb *SentenceBuffer) IsEmpty() bool {
	return sb.buf.Len() == 0
}

func (sb *SentenceBuffer) Reset() {
	sb.buf.Reset()
	sb.tokenCount = 0
	sb.firstSentence = true
	sb.minTokens = 2
	sb.lastEmitted = ""
}

func extractLastWord(s string) string {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}
