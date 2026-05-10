package prosody

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Processor sits between LLM output and TTS input
// It analyzes the text and adds natural prosody variations
type Processor struct {
	config  ProsodyConfig
	textCh  chan string
	resultCh chan ProcessedAudio
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type ProcessedAudio struct {
	Text        string
	Markers     []ProsodyMarker
	DurationMs  int
	AudioPackets []AudioPacket
}

type AudioPacket struct {
	Data   []byte
	IsFirst bool
	IsLast  bool
	WordIndex int
}

// NewProcessor creates a prosody processor
func NewProcessor(cfg ProsodyConfig) *Processor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Processor{
		config:   cfg,
		textCh:   make(chan string, 10),
		resultCh: make(chan ProcessedAudio, 10),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// ProcessText takes raw LLM output and applies prosody
func (p *Processor) ProcessText(text string) ProcessedAudio {
	// Clean up the text first
	cleanText := cleanupText(text)

	// Analyze prosody
	result := PredictProsody(cleanText, p.config)

	// Generate the "processed" text (with fillers if needed)
	processedText := generateProcessedText(result.Markers)

	return ProcessedAudio{
		Text:        processedText,
		Markers:     result.Markers,
		DurationMs:  result.EstimatedMs,
		AudioPackets: nil, // Will be filled during streaming
	}
}

// ProcessTextWithCallback processes text and calls callback for each "word" with timing hints
func (p *Processor) ProcessTextWithCallback(text string, onWord func(word string, marker ProsodyMarker) error) error {
	result := PredictProsody(text, p.config)

	currentWordIdx := 0
	for _, marker := range result.Markers {
		// Apply pause before word
		if marker.PauseBefore > 0 {
			time.Sleep(time.Duration(marker.PauseBefore) * time.Millisecond)
		}

		// Call the callback (which should trigger TTS for this word)
		if err := onWord(marker.Text, marker); err != nil {
			return err
		}

		// Apply pause after word
		if marker.PauseAfter > 0 {
			time.Sleep(time.Duration(marker.PauseAfter) * time.Millisecond)
		}

		currentWordIdx++
	}

	return nil
}

// cleanupText fixes common LLM output issues
func cleanupText(text string) string {
	// Remove markdown formatting
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "```", "")

	// Fix common issues
	text = strings.TrimSpace(text)

	// Remove incomplete sentences at the end (common LLM issue)
	lines := strings.Split(text, "\n")
	if len(lines) > 1 {
		lastLine := strings.TrimSpace(lines[len(lines)-1])
		if len(lastLine) < 20 && !strings.HasSuffix(lastLine, ".") {
			text = strings.Join(lines[:len(lines)-1], "\n")
		}
	}

	return text
}

// generateProcessedText creates clean text with any inserted fillers
func generateProcessedText(markers []ProsodyMarker) string {
	var words []string
	for _, m := range markers {
		words = append(words, m.Text)
	}
	return strings.Join(words, " ")
}

// CreateSSMLMarkers generates SSML-compatible markers if the TTS supports it
func CreateSSMLMarkers(markers []ProsodyMarker) string {
	return ToSSML(markers)
}

// AdaptiveProcessor adjusts prosody based on conversation context
type AdaptiveProcessor struct {
	baseConfig  ProsodyConfig
	mu          sync.RWMutex
	utteranceCount int
	lastComplexity float64
}

// NewAdaptiveProcessor creates a prosody processor that adapts to context
func NewAdaptiveProcessor(base ProsodyConfig) *AdaptiveProcessor {
	return &AdaptiveProcessor{
		baseConfig: base,
	}
}

// ProcessText applies prosody to text using current config
func (ap *AdaptiveProcessor) ProcessText(text string) ProsodyResult {
	cfg := ap.GetConfig()
	return PredictProsody(text, cfg)
}

// UpdateContext updates the processor with new context
func (ap *AdaptiveProcessor) UpdateContext(utterance string, durationMs int) {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	ap.utteranceCount++

	complexity := AnalyzeComplexity(utterance)

	// Smooth the complexity
	alpha := 0.3
	ap.lastComplexity = alpha*complexity + (1-alpha)*ap.lastComplexity
}

// GetConfig returns the current prosody config (potentially adjusted)
func (ap *AdaptiveProcessor) GetConfig() ProsodyConfig {
	ap.mu.RLock()
	defer ap.mu.RUnlock()

	cfg := ap.baseConfig

	// Adjust based on recent complexity
	if ap.lastComplexity > 0.6 {
		// High complexity - slow down, add more pauses
		cfg.BaseRate = ap.baseConfig.BaseRate * 0.85
		cfg.ClausePauseMs = ap.baseConfig.ClausePauseMs + 50
	} else if ap.lastComplexity < 0.3 {
		// Low complexity - can go faster
		cfg.BaseRate = ap.baseConfig.BaseRate * 1.1
		cfg.ClausePauseMs = ap.baseConfig.ClausePauseMs - 20
	}

	// Thinker mode kicks in after several exchanges (establishes rapport)
	if ap.utteranceCount > 5 && ap.lastComplexity > 0.4 {
		cfg.ThinkerMode = true
	}

	return cfg
}

// Integration with orchestrator - add this to your orchestrator.go

/*
Example usage in your orchestrator:

func (o *Orchestrator) GenerateWithProsody(ctx context.Context, session *ConversationSession) (string, error) {
	// Get raw response
	response, err := o.llm.Complete(ctx, session.GetContextCopy(), session.GetTools())
	if err != nil {
		return "", err
	}

	// Apply prosody
	prosodyCfg := prosody.DefaultConfig()
	prosodyCfg.BaseRate = 1.0
	prosodyCfg.ThinkerMode = o.config.UseThinkingFillers

	result := prosodyProcessor.ProcessText(response)

	// Return the prosody-enhanced text
	return result.Text, nil
}
*/

// VoiceStyleAdapter adapts prosody to specific voice characteristics
type VoiceStyleAdapter struct {
	style string
}

func NewVoiceStyleAdapter(voice string) *VoiceStyleAdapter {
	return &VoiceStyleAdapter{style: voice}
}

// AdjustConfig modifies prosody config based on voice style
func (vsa *VoiceStyleAdapter) AdjustConfig(cfg *ProsodyConfig) {
	switch vsa.style {
	case "friendly":
		cfg.WarmthFactor = 0.7
		cfg.ClausePauseMs += 50 // More natural pauses
		cfg.BaseRate = cfg.BaseRate * 0.95 // Slightly slower = warmer

	case "professional":
		cfg.WarmthFactor = 0.3
		cfg.ClausePauseMs -= 30 // More efficient
		cfg.BaseRate = cfg.BaseRate * 1.05 // Slightly faster

	case "empathetic":
		cfg.WarmthFactor = 0.8
		cfg.PauseDuration += 100 // More pauses = more empathy
		cfg.BasePitch -= 5 // Lower pitch = more serious

	case "energetic":
		cfg.WarmthFactor = 0.5
		cfg.BaseRate = cfg.BaseRate * 1.15 // Faster
		cfg.BasePitch += 10 // Higher energy
	}
}