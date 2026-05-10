package prosody

import (
	"regexp"
	"strings"
	"unicode"
)

type ProsodyConfig struct {
	BaseRate       float64   // 0.5 - 2.0, default 1.0
	BasePitch      float64   // Hz shift, default 0
	EmphasisLevel  float64   // 0.0 - 1.0
	PauseDuration  int       // ms between sentences
	ClausePauseMs  int       // ms between clauses
	WordPauseMs    int       // ms between words (for emphasis)
	ThinkerMode    bool      // add "um", "let me think" style fillers
	WarmthFactor   float64   // 0.0 - 1.0, adds warmth to voice
}

func DefaultConfig() ProsodyConfig {
	return ProsodyConfig{
		BaseRate:      1.0,
		BasePitch:     0,
		EmphasisLevel: 0.5,
		PauseDuration: 300,
		ClausePauseMs: 150,
		WordPauseMs:   50,
		ThinkerMode:   false,
		WarmthFactor:  0.3,
	}
}

type ProsodyMarker struct {
	Text         string
	WordIndex    int
	IsEmphasized bool
	PauseBefore  int // ms
	PauseAfter   int // ms
	PitchShift   float64
	RateModifier float64 // multiplier
}

type ProsodyResult struct {
	Markers     []ProsodyMarker
	FullText    string
	EstimatedMs int // estimated total duration
}

// FindClauseBoundaries finds natural clause boundaries using punctuation and conjunctions
func FindClauseBoundaries(text string) []int {
	clauseMarkers := []string{
		",", " but ", " however ", " although ", " because ", " since ",
		" therefore ", " so ", " and ", " or ", " which ", " who ", " where ",
		" when ", " if ", " while ", " whereas ",
	}

	boundaries := []int{0}
	lowerText := strings.ToLower(text)

	for _, marker := range clauseMarkers {
		idx := 0
		for {
			pos := strings.Index(lowerText[idx:], marker)
			if pos == -1 {
				break
			}
			actualPos := idx + pos
			if actualPos > 0 && actualPos < len(text)-1 {
				boundaries = append(boundaries, actualPos)
			}
			idx = actualPos + 1
		}
	}

	// Add sentence boundaries
	sentenceEnd := regexp.MustCompile(`[.!?]`)
	matches := sentenceEnd.FindAllStringIndex(text, -1)
	for _, m := range matches {
		boundaries = append(boundaries, m[1])
	}

	// Sort and dedupe
	unique := make(map[int]bool)
	for _, b := range boundaries {
		unique[b] = true
	}
	boundaries = nil
	for k := range unique {
		boundaries = append(boundaries, k)
	}
	slice := sortAndUnique(boundaries)

	return slice
}

func sortAndUnique(s []int) []int {
	seen := make(map[int]bool)
	result := []int{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	// Simple bubble sort for small slices
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i] > result[j] {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

// AnalyzeComplexity estimates cognitive load based on text features
func AnalyzeComplexity(text string) float64 {
	words := strings.Fields(text)
	if len(words) == 0 {
		return 0.5
	}

	complexity := 0.0

	// Long words indicate complexity
	longWords := 0
	for _, w := range words {
		if len(w) > 8 {
			longWords++
		}
	}
	complexity += float64(longWords) / float64(len(words)) * 0.3

	// Numbers and technical terms
	numbers := regexp.MustCompile(`\d+`)
	complexity += float64(len(numbers.FindAllString(text, -1))) / float64(len(words)) * 0.2

	// Question marks indicate thinking required
	complexity += float64(strings.Count(text, "?")) * 0.1

	// Complex conjunctions
	complexWords := []string{"however", "therefore", "consequently", "nevertheless", "furthermore"}
	lower := strings.ToLower(text)
	for _, w := range complexWords {
		if strings.Contains(lower, w) {
			complexity += 0.05
		}
	}

	if complexity > 1.0 {
		complexity = 1.0
	}

	return complexity
}

// PredictProsody takes text and returns marked-up version with prosody hints
func PredictProsody(text string, cfg ProsodyConfig) ProsodyResult {
	words := strings.Fields(text)
	markers := make([]ProsodyMarker, 0, len(words))

	complexity := AnalyzeComplexity(text)

	// Determine rate based on complexity
	// High complexity = slower speech
	baseRate := cfg.BaseRate
	if complexity > 0.6 {
		baseRate = cfg.BaseRate * 0.85 // Slow down
	} else if complexity < 0.3 {
		baseRate = cfg.BaseRate * 1.1 // Speed up slightly
	}

	// Find sentence boundaries
	sentences := splitIntoSentences(text)

	wordIndex := 0
	currentSentenceStart := 0

	for sentIdx, sentence := range sentences {
		sentWords := strings.Fields(sentence)
		if len(sentWords) == 0 {
			currentSentenceStart += len(sentence)
			continue
		}

		// Find most important word in sentence (usually first content word)
		importantWordIdx := findImportantWord(sentWords)

		// Calculate sentence-level pause
		pauseBefore := 0
		if sentIdx > 0 {
			pauseBefore = cfg.PauseDuration
		}

		// Analyze sentence complexity
		sentComplexity := AnalyzeComplexity(sentence)

		for i, word := range sentWords {
			isEmphasized := false
			rateMod := 1.0
			pitchShift := cfg.BasePitch

			// Emphasize important words
			if i == importantWordIdx {
				isEmphasized = true
				pitchShift += 15 // Slight pitch bump
			}

			// End of sentence - slight pitch drop
			if i == len(sentWords)-1 && sentIdx < len(sentences)-1 {
				pitchShift -= 10
			}

			// Complexity affects rate
			if sentComplexity > 0.7 {
				rateMod = 0.9
			}

			marker := ProsodyMarker{
				Text:         word,
				WordIndex:    wordIndex,
				IsEmphasized: isEmphasized,
				PauseBefore:  pauseBefore,
				PauseAfter:    0,
				PitchShift:   pitchShift,
				RateModifier: rateMod * baseRate,
			}

			// Add pauses after punctuation
			if len(word) > 0 {
				lastChar := rune(word[len(word)-1])
				if lastChar == '.' || lastChar == '?' || lastChar == '!' {
					marker.PauseAfter = cfg.PauseDuration
				} else if lastChar == ',' || lastChar == ';' || lastChar == ':' {
					marker.PauseAfter = cfg.ClausePauseMs
				}
			}

			markers = append(markers, marker)
			wordIndex++
			currentSentenceStart += len(word) + 1
			pauseBefore = 0 // Only first word gets sentence pause
		}
	}

	// Add thinker mode fillers if enabled
	if cfg.ThinkerMode && complexity > 0.5 {
		markers = addThinkingFillers(markers)
	}

	// Calculate estimated duration
	estimatedMs := estimateDuration(markers)

	return ProsodyResult{
		Markers:     markers,
		FullText:    text,
		EstimatedMs: estimatedMs,
	}
}

func splitIntoSentences(text string) []string {
	sentences := []string{}
	current := strings.Builder{}

	runes := []rune(text)
	for i, r := range runes {
		current.WriteRune(r)

		// Check for sentence end
		if r == '.' || r == '!' || r == '?' {
			// Look ahead to see if this is really the end
			if i+1 < len(runes) {
				next := runes[i+1]
				if unicode.IsUpper(next) || next == '"' || next == '\'' {
					sentences = append(sentences, current.String())
					current.Reset()
				}
			}
		}
	}

	if current.Len() > 0 {
		sentences = append(sentences, current.String())
	}

	if len(sentences) == 0 {
		return []string{text}
	}

	return sentences
}

func findImportantWord(words []string) int {
	// Skip function words, find first content word
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "must": true, "can": true, "to": true,
		"of": true, "in": true, "for": true, "on": true, "with": true,
		"at": true, "by": true, "from": true, "as": true, "into": true,
		"through": true, "during": true, "before": true, "after": true,
		"and": true, "but": true, "or": true, "nor": true, "so": true,
		"yet": true, "my": true, "your": true, "his": true, "her": true,
		"its": true, "our": true, "their": true, "this": true, "that": true,
	}

	for i, w := range words {
		lower := strings.ToLower(w)
		if !stopWords[lower] && len(w) > 2 {
			return i
		}
	}

	return 0
}

func addThinkingFillers(markers []ProsodyMarker) []ProsodyMarker {
	if len(markers) < 3 {
		return markers
	}

	// Insert filler after word 2-4
	insertPos := 2 + len(markers)%3

	fillers := []string{"Hmm", "Let me think", "Well", "So"}

	result := make([]ProsodyMarker, 0, len(markers)+2)
	for i, m := range markers {
		if i == insertPos {
			result = append(result, ProsodyMarker{
				Text:        fillers[len(markers)%len(fillers)],
				WordIndex:   m.WordIndex,
				PauseBefore: 200,
				PitchShift:  -20, // Lower pitch for thinking
			})
		}
		result = append(result, m)
	}

	return result
}

func estimateDuration(markers []ProsodyMarker) int {
	// Average English speaking rate: 150 words per minute = 10ms per word at 1x
	msPerWord := 60000 / 150

	total := 0
	for _, m := range markers {
		wordMs := int(float64(msPerWord) / m.RateModifier)
		total += wordMs
		total += m.PauseBefore
		total += m.PauseAfter
	}

	return total
}

// ToSSML converts prosody markers to SSML for TTS engines that support it
func ToSSML(markers []ProsodyMarker) string {
	var sb strings.Builder
	sb.WriteString("<speak>")

	for _, m := range markers {
		if m.PauseBefore > 0 {
			sb.WriteString("<break time=\"")
			sb.WriteString(formatMs(m.PauseBefore))
			sb.WriteString("ms\"/>")
		}

		if m.PitchShift != 0 || m.RateModifier != 1.0 {
			sb.WriteString("<prosody")
			added := false

			if m.PitchShift != 0 {
				sb.WriteString(" pitch=\"")
				if m.PitchShift > 0 {
					sb.WriteString("+")
				}
				sb.WriteString(formatMs(int(m.PitchShift)))
				sb.WriteString("Hz\"")
				added = true
			}

			if m.RateModifier != 1.0 {
				if !added {
					sb.WriteString(" ")
				}
				sb.WriteString("rate=\"")
				sb.WriteString(formatFloat(m.RateModifier))
				sb.WriteString("\"")
			}

			sb.WriteString(">")
			sb.WriteString(m.Text)
			sb.WriteString("</prosody>")
		} else {
			sb.WriteString(m.Text)
		}

		if m.PauseAfter > 0 {
			sb.WriteString("<break time=\"")
			sb.WriteString(formatMs(m.PauseAfter))
			sb.WriteString("ms\"/>")
		}

		sb.WriteString(" ")
	}

	sb.WriteString("</speak>")
	return sb.String()
}

func formatMs(ms int) string {
	if ms >= 1000 {
		return string(rune('0' + ms/1000))
	}
	return string(rune('0' + ms/100))
}

func formatFloat(f float64) string {
	if f == 1.0 {
		return "1.0"
	}
	if f < 1.0 {
		return strings.TrimSuffix(formatFloatInternal(f), "0")
	}
	return strings.TrimSuffix(formatFloatInternal(f), "0")
}

func formatFloatInternal(f float64) string {
	if f >= 1.0 {
		return "1.0"
	}
	return "0.9"
}

// SplitWords splits text into words for counting
func SplitWords(text string) int {
	return len(strings.Fields(text))
}