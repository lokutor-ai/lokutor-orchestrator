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
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
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

	if s, ok := stt.(interface{ SetSampleRate(int) }); ok {
		s.SetSampleRate(SampleRate)
	}

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

	config := orchestrator.DefaultConfig()
	config.Language = lang

	firstSpeaker := os.Getenv("FIRST_SPEAKER")
	if firstSpeaker == "bot" {
		config.FirstSpeaker = orchestrator.FirstSpeakerBot
	} else if firstSpeaker == "user" {
		config.FirstSpeaker = orchestrator.FirstSpeakerUser
	}

	fmt.Printf("Configured: STT=%s | LLM=%s | TTS=Lokutor\n", sttProviderName, llmProviderName)
	fmt.Printf("VAD Threshold: %.3f | Sample Rate: %dHz | Language: %s\n", config.BargeInVADThreshold, SampleRate, lang)
	fmt.Println("Voice Agent Started! Listening to microphone...")
	fmt.Println("Press Ctrl+C to exit")

	tts := ttsProvider.NewLokutorTTS(lokutorKey)

	vad := orchestrator.NewRMSVAD(config.BargeInVADThreshold, 800*time.Millisecond)
	vad.SetMinConfirmed(2)

	orch := orchestrator.NewWithVAD(stt, llm, tts, vad, config)

	session := orch.NewSessionWithDefaults("user_123")

	systemPrompt := "You are a helpful and concise voice assistant. " +
		"This is a real-time conversational phone call. " +
		"Use short sentences suitable for speech. " +
		"Sound naturally human: as a person would, feel free to use very subtle conversational fillers (like 'umm', 'mhm', or 'well...') and natural pauses only where they feel natural, but avoid overdoing it. " +
		"The goal is for the conversation to feel fluid, responsive, and less like a scripted AI."
	if lang == orchestrator.LanguageEs {
		systemPrompt = "Eres un asistente de voz útil y conciso. " +
			"Esta es una llamada telefónica conversacional en tiempo real. " +
			"Usa frases cortas adecuadas para el habla. " +
			"Suena naturalmente humano: como lo haría una persona, siéntete libre de usar pequeñas muletillas muy sutiles (como 'ehh', 'bueno', 'ajá') y pausas naturales solo donde se sientan apropiadas, pero sin exagerar. " +
			"El objetivo es que la conversación se sienta fluida, reactiva y menos como una IA programada."
	}
	orch.SetSystemPrompt(session, systemPrompt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := orch.NewManagedStream(ctx, session)
	stream.SetEchoSampleRates(SampleRate, SampleRate)
	defer stream.Close()

	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer mctx.Uninit()

	var playbackMu sync.Mutex
	var playbackBytes []byte
	var e2eLogged bool
	var preRolling bool = true
	const preRollSize = 44100 * 2 * 300 / 1000 // 300ms pre-roll

	chunkPool := sync.Pool{
		New: func() interface{} {
			return make([]byte, 8192)
		},
	}

	inputChan := make(chan []byte, 1024)
	go func() {
		for chunk := range inputChan {
			_ = stream.Write(chunk)
			chunkPool.Put(chunk)
		}
	}()

	playedChan := make(chan []byte, 1024)
	go func() {
		for chunk := range playedChan {
			stream.RecordPlayedOutput(chunk)
			chunkPool.Put(chunk)
		}
	}()

	onSamples := func(pOutput, pInput []byte, frameCount uint32) {
		if pInput != nil {
			buf := chunkPool.Get().([]byte)
			n := copy(buf, pInput)
			select {
			case inputChan <- buf[:n]:
			default:
				chunkPool.Put(buf)
			}
		}

		if pOutput != nil {
			bytesToRead := int(frameCount) * 2
			playbackMu.Lock()

			if preRolling {
				if len(playbackBytes) < preRollSize {
					playbackMu.Unlock()
					for i := range pOutput {
						pOutput[i] = 0
					}

					// Record silence for AEC
					buf := chunkPool.Get().([]byte)
					nc := copy(buf, pOutput)
					select {
					case playedChan <- buf[:nc]:
					default:
						chunkPool.Put(buf)
					}
					return
				}
				preRolling = false

			}

			if len(playbackBytes) == 0 {

				for i := range pOutput {
					pOutput[i] = 0
				}
			} else {
				if len(playbackBytes) < bytesToRead {

					copy(pOutput, playbackBytes)
					for i := len(playbackBytes); i < bytesToRead; i++ {
						pOutput[i] = 0
					}
					playbackBytes = nil
				} else {

					copy(pOutput, playbackBytes[:bytesToRead])
					playbackBytes = playbackBytes[bytesToRead:]
				}
			}

			if !preRolling {
				if !e2eLogged {
					bd := stream.GetLatencyBreakdown()
					if bd.UserToPlay > 0 || bd.UserToTTSFirstByte > 0 || bd.UserToLLM > 0 || bd.UserToSTT > 0 {
						fmt.Printf("\r\033[K⏱️ [LATENCY] user→stt=%dms stt=%dms user→llm=%dms llm=%dms user→tts_first=%dms llm→tts_first=%dms tts_total=%dms user→play=%dms\n",
							bd.UserToSTT, bd.STT, bd.UserToLLM, bd.LLM, bd.UserToTTSFirstByte, bd.LLMToTTSFirstByte, bd.TTSTotal, bd.UserToPlay)
						e2eLogged = true
					}
				}
				stream.NotifyAudioPlayed()
			}
			playbackMu.Unlock()

			// Record EVERYTHING we play (voice AND silence) so AEC timeline stays perfectly synchronized
			buf := chunkPool.Get().([]byte)
			nc := copy(buf, pOutput)
			select {
			case playedChan <- buf[:nc]:
			default:
				chunkPool.Put(buf)
			}
		}
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Duplex)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = 1
	deviceConfig.Playback.Format = malgo.FormatS16
	deviceConfig.Playback.Channels = 1
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1

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

	go func() {
		currentGeneration := 0
		for event := range stream.Events() {
			switch event.Type {
			case orchestrator.UserSpeaking:
				fmt.Printf("\r\033[K🎤 [USER] Speaking...\n")

			case orchestrator.Interrupted:
				playbackMu.Lock()
				playbackBytes = nil
				preRolling = true
				currentGeneration = event.Generation
				playbackMu.Unlock()
				fmt.Printf("\r\033[K🛑 [INTERRUPTED] User started talking (gen: %d).\n", currentGeneration)

			case orchestrator.UserStopped:
				fmt.Printf("\r\033[K⌛ [STT] Processing...\n")
			case orchestrator.TranscriptFinal:
				fmt.Printf("\r\033[K📝 [TRANSCRIPT] %s\n", event.Data.(string))
				raw, proc := stream.ExportLastUserAudio()
				if raw != nil {
					ts := time.Now().Format("20060102-150405")
					rawPath := fmt.Sprintf("/tmp/lokutor_user_raw_%s.wav", ts)
					procPath := fmt.Sprintf("/tmp/lokutor_user_processed_%s.wav", ts)
					_ = os.WriteFile(rawPath, audio.NewWavBuffer(raw, SampleRate), 0644)
					_ = os.WriteFile(procPath, audio.NewWavBuffer(proc, SampleRate), 0644)
					fmt.Printf("\r\033[K💾 Saved user audio: %s (raw), %s (processed)\n", rawPath, procPath)
				}

			case orchestrator.BotThinking:
				fmt.Printf("\r\033[K🧠 [LLM] Thinking...\n")
			case orchestrator.BotResponse:
				if resp, ok := event.Data.(string); ok {
					fmt.Printf("\r\033[K💬 [AGENT] %s\n", resp)
				}
			case orchestrator.BotSpeaking:
				latency := stream.GetLatency()
				if latency > 0 {
					fmt.Printf("\r\033[K🔊 [TTS] Speaking... (latency: %dms)\n", latency)
				} else {
					fmt.Printf("\r\033[K🔊 [TTS] Speaking...\n")
				}
			case orchestrator.AudioChunk:
				if event.Generation < currentGeneration {

					continue
				}
				chunk := event.Data.([]byte)
				playbackMu.Lock()
				playbackBytes = append(playbackBytes, chunk...)
				playbackMu.Unlock()

			case orchestrator.ErrorEvent:
				fmt.Printf("\r\033[K❌ [ERROR] %v\n", event.Data)
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	sigReceived := <-sig
	fmt.Printf("\nShutting down (signal=%v)...\n", sigReceived)

	_ = device.Stop()
	stream.Close()
	time.Sleep(50 * time.Millisecond)
}
