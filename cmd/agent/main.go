package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/joho/godotenv"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	llmProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/llm"
	sttProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
	ttsProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/tts"
)

const (
	SampleRate = 44100
	Channels   = 1
)

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Note: No .env file found, using system environment variables")
	}

	groqKey := os.Getenv("GROQ_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	googleKey := os.Getenv("GOOGLE_API_KEY")
	deepgramKey := os.Getenv("DEEPGRAM_API_KEY")
	assemblyKey := os.Getenv("ASSEMBLYAI_API_KEY")
	lokutorKey := os.Getenv("LOKUTOR_API_KEY")

	sttProviderName := os.Getenv("STT_PROVIDER")
	if sttProviderName == "" {
		sttProviderName = "groq"
	}
	llmProviderName := os.Getenv("LLM_PROVIDER")
	if llmProviderName == "" {
		llmProviderName = "groq"
	}

	lang := orchestrator.Language(os.Getenv("AGENT_LANGUAGE"))
	if lang == "" {
		lang = orchestrator.LanguageEs
	}

	if lokutorKey == "" {
		log.Fatal("Error: LOKUTOR_API_KEY must be set.")
	}

	// STT Selection
	var stt orchestrator.STTProvider
	switch sttProviderName {
	case "openai":
		if openaiKey == "" {
			log.Fatal("Error: OPENAI_API_KEY must be set for openai STT")
		}
		stt = sttProvider.NewOpenAISTT(openaiKey, "whisper-1")
	case "deepgram":
		if deepgramKey == "" {
			log.Fatal("Error: DEEPGRAM_API_KEY must be set for deepgram STT")
		}
		stt = sttProvider.NewDeepgramSTT(deepgramKey)
	case "assemblyai":
		if assemblyKey == "" {
			log.Fatal("Error: ASSEMBLYAI_API_KEY must be set for assemblyai STT")
		}
		stt = sttProvider.NewAssemblyAISTT(assemblyKey)
	case "groq":
		fallthrough
	default:
		if groqKey == "" {
			log.Fatal("Error: GROQ_API_KEY must be set for groq STT")
		}
		groqModel := os.Getenv("GROQ_STT_MODEL")
		if groqModel == "" {
			groqModel = "whisper-large-v3"
		}
		stt = sttProvider.NewGroqSTT(groqKey, groqModel)
	}

	// Set sample rate if supported
	if s, ok := stt.(interface{ SetSampleRate(int) }); ok {
		s.SetSampleRate(SampleRate)
	}

	// LLM Selection
	var llm orchestrator.LLMProvider
	switch llmProviderName {
	case "openai":
		if openaiKey == "" {
			log.Fatal("Error: OPENAI_API_KEY must be set for openai LLM")
		}
		llm = llmProvider.NewOpenAILLM(openaiKey, "gpt-4o")
	case "anthropic":
		if anthropicKey == "" {
			log.Fatal("Error: ANTHROPIC_API_KEY must be set for anthropic LLM")
		}
		llm = llmProvider.NewAnthropicLLM(anthropicKey, "claude-3-5-sonnet-20241022")
	case "google":
		if googleKey == "" {
			log.Fatal("Error: GOOGLE_API_KEY must be set for google LLM")
		}
		llm = llmProvider.NewGoogleLLM(googleKey, "gemini-1.5-flash")
	case "groq":
		fallthrough
	default:
		if groqKey == "" {
			log.Fatal("Error: GROQ_API_KEY must be set for groq LLM")
		}
		llm = llmProvider.NewGroqLLM(groqKey, "llama-3.3-70b-versatile")
	}

	fmt.Printf("Configured: STT=%s | LLM=%s | TTS=Lokutor\n", sttProviderName, llmProviderName)
	fmt.Printf("VAD Threshold: %.3f | Sample Rate: %dHz | Language: %s\n", 0.02, SampleRate, lang)
	fmt.Println("Voice Agent Started! Listening to microphone...")
	fmt.Println("Press Ctrl+C to exit")

	tts := ttsProvider.NewLokutorTTS(lokutorKey)

	vad := orchestrator.NewRMSVAD(0.02, 500*time.Millisecond) // Lowered threshold to 0.02 for better sensitivity

	config := orchestrator.DefaultConfig()
	config.Language = lang
	orch := orchestrator.NewWithVAD(stt, llm, tts, vad, config)

	session := orch.NewSessionWithDefaults("user_123")

	systemPrompt := "You are a helpful and concise voice assistant. Use short sentences suitable for speech."
	if lang == orchestrator.LanguageEs {
		systemPrompt = "Eres un asistente de voz útil y conciso. Usa frases cortas adecuadas para el habla."
	}
	orch.SetSystemPrompt(session, systemPrompt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := orch.NewManagedStream(ctx, session)
	defer stream.Close()

	// 2. Setup Audio Engine (malgo)
	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer mctx.Uninit()

	// Buffer for simple playback coordination
	var playbackMu sync.Mutex
	var playbackBytes []byte

	var botPlayingMu sync.Mutex
	var lastPlayedAt time.Time

	var rmsMu sync.Mutex
	lastRMS := 0.0

	onSamples := func(pOutput, pInput []byte, frameCount uint32) {
		if pInput != nil {
			// Calculate RMS for debugging/logging
			var sum float64
			for i := 0; i < len(pInput)-1; i += 2 {
				sample := int16(pInput[i]) | (int16(pInput[i+1]) << 8)
				f := float64(sample) / 32768.0
				sum += f * f
			}
			rms := math.Sqrt(sum / float64(len(pInput)/2))
			rmsMu.Lock()
			lastRMS = rms
			rmsMu.Unlock()

			// Heuristic: If bot is speaking, it's probably picking up its own audio.
			// Increase threshold temporarily to avoid self-interruption.
			effectiveThreshold := 0.02
			botPlayingMu.Lock()
			// If we played audio in the last 400ms, we consider the bot as "active"
			// to account for room reverb, hardware latency, and delivery delays.
			isActuallyPlaying := time.Since(lastPlayedAt) < 400*time.Millisecond
			if isActuallyPlaying {
				effectiveThreshold = 0.20 // Higher threshold to filter out loud speaker output
			}
			botPlayingMu.Unlock()

			// Check against threshold
			if rms > effectiveThreshold {
				_ = stream.Write(pInput)
			} else {
				// Send silence to the VAD so it can track silence duration even while bot speaks
				_ = stream.Write(make([]byte, len(pInput)))
			}
		}
		if pOutput != nil {
			playbackMu.Lock()
			n := copy(pOutput, playbackBytes)
			playbackBytes = playbackBytes[n:]

			// If we played something, update the timestamp
			if n > 0 {
				botPlayingMu.Lock()
				lastPlayedAt = time.Now()
				botPlayingMu.Unlock()
			}

			// Fill remaining with silence if playbackBytes was shorter than pOutput
			if n < len(pOutput) {
				for i := n; i < len(pOutput); i++ {
					pOutput[i] = 0
				}
			}
			playbackMu.Unlock()
		}
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Duplex)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = 1
	deviceConfig.Playback.Format = malgo.FormatS16
	deviceConfig.Playback.Channels = 1
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1 // Better compatibility on some systems

	device, err := malgo.InitDevice(mctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: onSamples,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		log.Fatal(err)
	}

	// Visual feedback for microphone levels
	go func() {
		for {
			rmsMu.Lock()
			level := lastRMS
			rmsMu.Unlock()

			if level >= 0.0 {
				meter := ""
				dots := int(level * 500) // Multiply by more to see smaller fluctuations
				if dots > 40 {
					dots = 40
				}
				for i := 0; i < dots; i++ {
					meter += "|"
				}
				fmt.Printf("\r[MIC ENERGY: %-40s] RMS: %.5f", meter, level)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	go func() {
		for event := range stream.Events() {
			switch event.Type {
			case orchestrator.UserSpeaking:
				fmt.Printf("\r\033[K🎤 [USER] Speaking...\n")
			case orchestrator.UserStopped:
				fmt.Printf("\r\033[K⌛ [STT] Processing...\n")
			case orchestrator.TranscriptFinal:
				fmt.Printf("\r\033[K📝 [TRANSCRIPT] %s\n", event.Data.(string))
			case orchestrator.BotThinking:
				fmt.Printf("\r\033[K🧠 [LLM] Thinking...\n")
			case orchestrator.BotSpeaking:
				fmt.Printf("\r\033[K🔊 [TTS] Speaking...\n")
			case orchestrator.AudioChunk:
				chunk := event.Data.([]byte)
				playbackMu.Lock()
				playbackBytes = append(playbackBytes, chunk...)
				playbackMu.Unlock()
			case orchestrator.Interrupted:
				fmt.Printf("\r\033[K🛑 [INTERRUPTED] User started talking.\n")
				playbackMu.Lock()
				playbackBytes = nil
				playbackMu.Unlock()
			case orchestrator.ErrorEvent:
				fmt.Printf("\r\033[K❌ [ERROR] %v\n", event.Data)
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Printf("\nShutting down...\n")
}
