package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

func (ms *ManagedStream) WriteControl(data []byte) error {
	select {
	case ms.controlChan <- data:
		return nil
	default:
		ms.logger.Warn("WriteControl dropped", "len", len(data))
		return nil
	}
}

func (ms *ManagedStream) handleControl(data []byte) {
	var msg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		ms.logger.Warn("invalid control message", "err", err)
		return
	}

	ms.mu.Lock()
	state := ms.state
	ms.mu.Unlock()

	switch msg.Type {
	case "vad_speech_start":
		ms.mu.Lock()
		ms.clientVAD = true
		ms.mu.Unlock()
		ms.onVADStart(state)

	case "vad_speech_end":
		ms.mu.Lock()
		ms.clientVAD = true
		ms.mu.Unlock()
		ms.onVADEnd(state)

	case "vad_speech_start_server":
		ms.mu.Lock()
		ms.clientVAD = false
		ms.mu.Unlock()
		ms.onVADStart(state)

	default:
		ms.logger.Debug("unknown control message type", "type", msg.Type)
	}
}

func (ms *ManagedStream) runStreamingLLM(ctx context.Context, provider StreamingLLMProvider, gen int) {
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()

	type toolRes struct {
		tc     ToolCallEventData
		result string
	}
	var toolResults []toolRes
	var toolMu sync.Mutex

	ttsQueue := make(chan string, 16)
	ttsWg := sync.WaitGroup{}
	ttsWg.Add(1)
	go func() {
		defer ttsWg.Done()
		for text := range ttsQueue {
			ms.speakText(ctx, text, gen)
		}
	}()

	var pendingSentence strings.Builder

	flushSentence := func() {
		s := strings.TrimSpace(pendingSentence.String())
		if s == "" {
			return
		}
		ttsQueue <- s
		pendingSentence.Reset()
	}

	var toolWg sync.WaitGroup

	_, err := provider.StreamComplete(ctx, messages, ms.session.GetTools(),
		func(chunk string) error {
			fullText.WriteString(chunk)
			pendingSentence.WriteString(chunk)

			if ms.llmEndTime.IsZero() {
				ms.llmEndTime = time.Now()
			}

			buf := pendingSentence.String()

			// Flush on sentence-ending punctuation
			for i, c := range buf {
				if c == '.' || c == '!' || c == '?' {
					sentence := strings.TrimSpace(buf[:i+1])
					if sentence != "" {
						ttsQueue <- sentence
					}
					rest := strings.TrimSpace(buf[i+1:])
					pendingSentence.Reset()
					pendingSentence.WriteString(rest)
					break
				}
			}

			return nil
		},
		func(tc ToolCallEventData) error {
			// Check for infinite tool loop
			if !ms.session.RecordToolCall(tc.Name) {
				ms.emit(ErrorEvent, fmt.Sprintf("Tool loop detected: %s called too many times. Aborting to prevent infinite retry.", tc.Name))
				return fmt.Errorf("tool loop detected: %s", tc.Name)
			}

			hasToolCalls = true
			ms.emit(ToolCall, tc)

			filler := strings.TrimSpace(fullText.String())
			if filler != "" {
				go func(t string) {
					sCtx, sCancel := context.WithCancel(ctx)
					defer sCancel()
					ms.speakText(sCtx, t, gen)
				}(filler)
				fullText.Reset()
			}

			toolWg.Add(1)
			go func(tcData ToolCallEventData) {
				defer toolWg.Done()

				handler, ok := ms.orch.toolHandlers[tcData.Name]
				result := "Error: tool not found"
				if ok {
					r, err := handler(tcData.Arguments)
					if err == nil {
						result = r
					} else {
						result = fmt.Sprintf("Error: %v", err)
					}
				}

				toolMu.Lock()
				toolResults = append(toolResults, toolRes{tc: tcData, result: result})
				toolMu.Unlock()
			}(tc)

			return nil
		},
	)

	toolWg.Wait()

	flushSentence()
	close(ttsQueue)
	ttsWg.Wait()

	if err != nil {
		ms.mu.Lock()
		ms.state = StateIdle
		ms.mu.Unlock()
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	response := strings.TrimSpace(fullText.String())

	if response != "" && !hasToolCalls {
		ms.session.AddMessage("assistant", response)
		ms.emit(BotResponse, response)
	}

	if hasToolCalls {
		var tcData []interface{}
		for _, tr := range toolResults {
			tcData = append(tcData, map[string]interface{}{
				"id":   tr.tc.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tr.tc.Name,
					"arguments": tr.tc.Arguments,
				},
			})
			ms.emit(ToolResult, tr)
		}

		ms.session.AddMessageRaw(Message{
			Role:      "assistant",
			Content:   response,
			ToolCalls: tcData,
		})

		for _, tr := range toolResults {
			ms.session.AddMessageRaw(Message{
				Role:       "tool",
				Content:    tr.result,
				ToolCallID: tr.tc.CallID,
			})
		}

		go func() {
			freshCtx, c := context.WithCancel(ms.ctx)
			defer c()
			ms.runLLMAndTTS(freshCtx, "")
		}()
	}
}

func (ms *ManagedStream) getState() StreamState {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.state
}

func (ms *ManagedStream) checkResponseCache(transcript string) (string, []byte, bool) {
	if ms.responseCache == nil {
		return "", nil, false
	}
	key := CacheKeyFor(transcript, ms.lastUserText)
	response, audio, ok := ms.responseCache.Get(key)
	if ok {
		ms.logger.Info("Response cache hit", "key", key)
		ms.emit(CacheHit, key)
	}
	return response, audio, ok
}

func (ms *ManagedStream) cacheResponse(transcript, response string, audio []byte) {
	if ms.responseCache == nil {
		return
	}
	key := CacheKeyFor(transcript, ms.lastUserText)
	ms.responseCache.Set(key, response, audio, 5*time.Minute)
}

func (ms *ManagedStream) summarizeContextIfNeeded() {
	if !ms.orch.config.ContextSummarization {
		return
	}
	if !ms.session.NeedsSummarization() {
		return
	}

	messages := ms.session.GetContextCopy()
	var turnsToSummarize []Message
	for _, msg := range messages {
		if msg.Role == "system" && strings.HasPrefix(msg.Content, "[Summary") {
			continue
		}
		if msg.Role == "user" || msg.Role == "assistant" {
			turnsToSummarize = append(turnsToSummarize, msg)
		}
	}

	if len(turnsToSummarize) < 4 {
		return
	}

	summarizeCount := len(turnsToSummarize) / 2
	oldTurns := turnsToSummarize[:summarizeCount]

	var sb strings.Builder
	for _, msg := range oldTurns {
		sb.WriteString(msg.Role + ": " + msg.Content + "\n")
	}

	go func(oldConversation string) {
		prompt := ms.orch.config.SummarizationPrompt + "\n\n" + oldConversation
		summaryMessages := []Message{
			{Role: "system", Content: "You generate concise summaries of conversations. Keep key facts and context, max 3 sentences."},
			{Role: "user", Content: prompt},
		}

		summary, err := ms.orch.llm.Complete(context.Background(), summaryMessages, nil)
		if err != nil || summary == "" {
			return
		}

		ms.session.SummarizeContext(summary, ms.session.MaxMessages/2)
	}(sb.String())
}

func (ms *ManagedStream) SetClientVAD(enabled bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.clientVAD = enabled
	ms.logger.Info("Client VAD mode", "enabled", enabled)
}

func (ms *ManagedStream) IsClientVAD() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.clientVAD
}
