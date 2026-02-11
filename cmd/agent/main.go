package main

import (
	"context"
	"fmt"
	"log"
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

	vad := orchestrator.NewRMSVAD(0.02, 500*time.Millisecond)
	vad.SetMinConfirmed(3) // Require ~30-50ms to trigger barge-in - very snappy

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

	var rmsMu sync.Mutex
	lastRMS := 0.0

	onSamples := func(pOutput, pInput []byte, frameCount uint32) {
		if pInput != nil {
			// Write to the stream - Echo Guard and VAD are handled internally
			_ = stream.Write(pInput)

			// Update the mic energy meter RMS data
			rmsMu.Lock()
			lastRMS = vad.LastRMS()
			rmsMu.Unlock()
		}
		if pOutput != nil {
			playbackMu.Lock()

			// Micro-cushion: only start playing if we have enough to fill at least half a frame.
			// This prevents stuttering when network packets are smaller than the hardware buffer.
			if len(playbackBytes) < len(pOutput)/2 && len(playbackBytes) < 2048 {
				for i := range pOutput {
					pOutput[i] = 0
				}
				playbackMu.Unlock()
				return
			}

			// Consume audio from buffer as soon as it arrives
			bytesToCopy := len(pOutput)
			if len(playbackBytes) < bytesToCopy {
				bytesToCopy = len(playbackBytes)
			}
			// Align to 2-byte boundary (16-bit)
			bytesToCopy -= bytesToCopy % 2

			n := copy(pOutput[:bytesToCopy], playbackBytes[:bytesToCopy])
			playbackBytes = playbackBytes[n:]

			// If we played something, update the Echo Guard timer
			if n > 0 {
				stream.NotifyAudioPlayed()
			}

			// Fill remaining with silence
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
