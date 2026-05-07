package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

type SpeculativeState string

const (
	SpecIdle      SpeculativeState = "idle"
	SpecWorking   SpeculativeState = "working"
	SpecReady     SpeculativeState = "ready"
	SpecCommitted SpeculativeState = "committed"
	SpecFailed    SpeculativeState = "failed"
)

type SpeculativeCandidate struct {
	mu sync.Mutex

	state          SpeculativeState
	interimTranscript string
	finalTranscript   string
	speculatedText    string
	firstSentence     string
	firstSentenceAudio []byte
	confidence        float64

	createdAt time.Time
	ttsReady  bool

	acceptOnFinal bool
}

type Speculator struct {
	mu sync.Mutex

	enabled bool
	orch    *Orchestrator
	session *ConversationSession

	candidate     *SpeculativeCandidate
	thinkingWords int

	speculativeProvider LLMProvider
}

func NewSpeculator(orch *Orchestrator, session *ConversationSession, enabled bool) *Speculator {
	s := &Speculator{
		enabled:       enabled,
		orch:          orch,
		session:       session,
		thinkingWords: 3,
	}
	return s
}

func (sp *Speculator) OnInterimTranscript(ctx context.Context, transcript string) *SpeculativeCandidate {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if !sp.enabled || sp.speculativeProvider == nil {
		return nil
	}

	clean := strings.TrimSpace(transcript)
	words := strings.Fields(clean)
	if len(words) < sp.thinkingWords {
		return nil
	}

	if sp.candidate != nil {
		sp.candidate.mu.Lock()
		state := sp.candidate.state
		sp.candidate.mu.Unlock()

		if state == SpecWorking || state == SpecReady {
			return sp.candidate
		}
	}

	if sp.orch.llm == nil {
		return nil
	}

	candidate := &SpeculativeCandidate{
		state:             SpecWorking,
		interimTranscript: clean,
		createdAt:         time.Now(),
	}

	sp.candidate = candidate

	go sp.generateSpeculation(ctx, candidate, clean)

	return candidate
}

func (sp *Speculator) generateSpeculation(ctx context.Context, candidate *SpeculativeCandidate, interimText string) {
	messages := sp.session.GetContextCopy()

	partialPrompt := interimText

	specMessages := make([]Message, len(messages))
	copy(specMessages, messages)
	specMessages = append(specMessages, Message{
		Role:    "system",
		Content: "PREDICT how the user's complete question will be and draft a brief response. The user is still speaking; guess their intent from the partial text. Keep your response under 20 words and conversational.",
	})
	specMessages = append(specMessages, Message{
		Role:    "user",
		Content: fmt.Sprintf("(still speaking) %s", partialPrompt),
	})

	startTime := time.Now()
	response, err := sp.speculativeProvider.Complete(ctx, specMessages, nil)

	candidate.mu.Lock()
	defer candidate.mu.Unlock()

	if err != nil || response == "" {
		candidate.state = SpecFailed
		return
	}

	genTime := time.Since(startTime)
	_ = genTime

	response = strings.TrimSpace(response)

	firstSentence := extractFirstSentence(response)
	if firstSentence == "" {
		firstSentence = response
	}

	candidate.speculatedText = response
	candidate.firstSentence = firstSentence
	candidate.confidence = speculateConfidence(interimText, response)

	if candidate.confidence > 0.5 {
		candidate.state = SpecReady
	} else {
		candidate.state = SpecFailed
	}
}

func (sp *Speculator) OnFinalTranscript(ctx context.Context, finalText string) *SpeculativeCandidate {
	sp.mu.Lock()
	candidate := sp.candidate
	sp.mu.Unlock()

	if candidate == nil {
		return nil
	}

	candidate.mu.Lock()
	defer candidate.mu.Unlock()

	candidate.finalTranscript = finalText

	if candidate.state != SpecReady {
		return nil
	}

	similarity := specSimilarity(candidate.interimTranscript, finalText)
	candidate.confidence = (candidate.confidence + similarity) / 2

	if candidate.confidence > 0.6 {
		candidate.acceptOnFinal = true
		return candidate
	}

	candidate.state = SpecFailed
	return nil
}

func (sp *Speculator) PreSynthesizeFirstSentence(ctx context.Context, candidate *SpeculativeCandidate) {
	candidate.mu.Lock()
	if candidate.state != SpecReady || candidate.ttsReady {
		candidate.mu.Unlock()
		return
	}
	sentence := candidate.firstSentence
	candidate.mu.Unlock()

	if sentence == "" {
		return
	}

	audio, err := sp.orch.Synthesize(ctx, sentence, sp.session.GetCurrentVoice(), sp.session.GetCurrentLanguage())
	if err != nil {
		return
	}

	candidate.mu.Lock()
	candidate.firstSentenceAudio = audio
	candidate.ttsReady = true
	candidate.mu.Unlock()
}

func (sp *Speculator) Reset() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.candidate = nil
}

func speculateConfidence(partial string, response string) float64 {
	partialWords := strings.Fields(strings.ToLower(partial))
	if len(partialWords) == 0 {
		return 0
	}

	responseLower := strings.ToLower(response)
	matchCount := 0
	for _, w := range partialWords {
		w = strings.Trim(w, ".,!?¿¡")
		if len(w) <= 2 {
			continue
		}
		if strings.Contains(responseLower, w) {
			matchCount++
		}
	}

	return float64(matchCount) / float64(len(partialWords))
}

func specSimilarity(a, b string) float64 {
	wordsA := wordSet(a)
	wordsB := wordSet(b)

	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}

	intersection := 0
	for w := range wordsA {
		if wordsB[w] {
			intersection++
		}
	}

	union := len(wordsA) + len(wordsB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func wordSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,!?¿¡;:")
		if len(w) >= 2 {
			set[w] = true
		}
	}
	return set
}

func extractFirstSentence(text string) string {
	runes := []rune(strings.TrimSpace(text))
	end := -1

	for i, r := range runes {
		if r == '.' || r == '!' || r == '?' || r == '¿' || r == '¡' {
			if i > 0 {
				prev := runeAt(runes, i-1)
				if unicode.IsLetter(prev) || unicode.IsDigit(prev) {
					before := string(runes[:i])
					lastWord := extractLastWord(before)
					if !isAbbreviation(lastWord) {
						end = i + 1
						break
					}
				}
			}
		}
	}

	if end > 0 {
		return strings.TrimSpace(string(runes[:end]))
	}

	if len(runes) > 100 {
		return strings.TrimSpace(string(runes[:100])) + "."
	}

	return strings.TrimSpace(text)
}

func runeAt(runes []rune, i int) rune {
	if i >= 0 && i < len(runes) {
		return runes[i]
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (sp *Speculator) SetSpeculativeProvider(provider LLMProvider) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.speculativeProvider = provider
}

func (sp *Speculator) GetState() SpeculativeState {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.candidate == nil {
		return SpecIdle
	}
	sp.candidate.mu.Lock()
	defer sp.candidate.mu.Unlock()
	return sp.candidate.state
}

func (sp *Speculator) GetCandidate() *SpeculativeCandidate {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.candidate
}

func (sp *Speculator) HasAccepted() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.candidate == nil {
		return false
	}
	sp.candidate.mu.Lock()
	defer sp.candidate.mu.Unlock()
	return sp.candidate.acceptOnFinal
}

func (sp *Speculator) AcceptAndConsume() (string, []byte) {
	sp.mu.Lock()
	candidate := sp.candidate
	sp.candidate = nil
	sp.mu.Unlock()

	if candidate == nil {
		return "", nil
	}

	candidate.mu.Lock()
	defer candidate.mu.Unlock()

	return candidate.firstSentence, candidate.firstSentenceAudio
}
