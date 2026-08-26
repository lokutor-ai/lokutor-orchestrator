package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// toolHandlerResult carries the result of a server-side tool handler invocation,
// including the error if the handler failed.
type toolHandlerResult struct {
	res string
	err error
}

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

// parseToolCallMarker detects the "[TOOL_CALLS] <json>" / "[TOOL_CALL] <json>"
// marker that non-streaming LLM providers (Anthropic, OpenAI) return when
// they want to call a tool but have no StreamComplete/onToolCall callback to
// invoke directly, and parses it back into ToolCallEventData. The marker's
// JSON is an array of {id, type:"function", function:{name, arguments}}
// objects; arguments may be a raw JSON value (Anthropic) or a JSON-encoded
// string (OpenAI) — both are normalized to a plain argument string here.
func parseToolCallMarker(response string) ([]ToolCallEventData, bool) {
	if !strings.HasPrefix(response, "[TOOL_CALL") {
		return nil, false
	}
	tagEnd := strings.Index(response, "] ")
	if tagEnd < 0 {
		return nil, false
	}
	raw := strings.TrimSpace(response[tagEnd+2:])

	var parsed []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, false
	}

	calls := make([]ToolCallEventData, 0, len(parsed))
	for i, p := range parsed {
		if p.Function.Name == "" {
			continue
		}
		argsStr := string(p.Function.Arguments)
		var unquoted string
		if json.Unmarshal(p.Function.Arguments, &unquoted) == nil {
			argsStr = unquoted // was a JSON-encoded string (OpenAI shape)
		}
		callID := p.ID
		if callID == "" {
			callID = fmt.Sprintf("%s_%d", p.Function.Name, i)
		}
		calls = append(calls, ToolCallEventData{
			Name:      p.Function.Name,
			Arguments: argsStr,
			CallID:    callID,
		})
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// dispatchToolCall runs one tool call — a registered server-side handler
// (15s timeout) or, if none is registered, waits for a client to submit a
// result via SubmitToolResult (10s timeout) — and returns the result string.
// Shared by the streaming tool-call path (invoked per-call as they arrive)
// and the non-streaming path (invoked for a batch parsed from a
// [TOOL_CALLS] marker), so both dispatch and time out identically.
func (ms *ManagedStream) dispatchToolCall(ctx context.Context, tcData ToolCallEventData) string {
	if handler, ok := ms.orch.toolHandlers[tcData.Name]; ok {
		hrCh := make(chan toolHandlerResult, 1)
		go func() {
			r, err := handler(tcData.Arguments)
			hrCh <- toolHandlerResult{res: r, err: err}
		}()
		select {
		case hr := <-hrCh:
			if hr.err == nil {
				return hr.res
			}
			return fmt.Sprintf(`{"error": %s}`, jsonQuote(hr.err.Error()))
		case <-time.After(15 * time.Second):
			return `{"error": "tool handler timed out after 15 seconds"}`
		case <-ms.ctx.Done():
			return `{"error": "cancelled"}`
		}
	}

	// Client-side tool: create a channel and wait for the client to respond.
	ch := make(chan string, 1)
	ms.clientToolResultsMu.Lock()
	ms.clientToolResults[tcData.CallID] = ch
	ms.clientToolResultsMu.Unlock()
	defer func() {
		ms.clientToolResultsMu.Lock()
		delete(ms.clientToolResults, tcData.CallID)
		ms.clientToolResultsMu.Unlock()
	}()

	resultCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	select {
	case res := <-ch:
		return res
	case <-resultCtx.Done():
		return `{"error": "client tool request timed out after 10 seconds"}`
	case <-ms.ctx.Done():
		return `{"error": "cancelled"}`
	}
}

// handleNonStreamingToolCalls executes a batch of tool calls parsed from a
// [TOOL_CALLS] marker (the non-streaming Anthropic/OpenAI path) and speaks
// the model's follow-up answer. Mirrors runStreamingLLM's tool-dispatch
// pattern — parallel goroutines via dispatchToolCall, a filler phrase while
// tools run, per-call loop guard — so behavior is consistent regardless of
// which LLM provider is configured. Does not recurse into a further round of
// tool calls if the follow-up answer is itself another marker; that matches
// the existing limitation of the streaming path's own round-2 fallback, and
// parseToolCallMarker guards against speaking that raw JSON either way.
func (ms *ManagedStream) handleNonStreamingToolCalls(ctx context.Context, gen int, userTranscript string, calls []ToolCallEventData) {
	fillerPhrase := toolFillerForLang(ms.session.GetCurrentLanguage())
	if fillerPhrase != "" {
		go func(t string) {
			sCtx, sCancel := context.WithCancel(ctx)
			defer sCancel()
			ms.speakText(sCtx, t, gen)
		}(fillerPhrase)
	}

	type toolRes struct {
		TC     ToolCallEventData
		Result string
	}
	var results []toolRes
	var resMu sync.Mutex
	var wg sync.WaitGroup

	for _, tc := range calls {
		ms.emit(ToolCall, tc)
		if !ms.session.RecordToolCall(tc.Name) {
			ms.emit(ErrorEvent, fmt.Sprintf("Tool loop detected: %s called too many times. Aborting to prevent infinite retry.", tc.Name))
			ms.mu.Lock()
			if ms.state != StateInterrupted {
				ms.state = StateIdle
			}
			ms.mu.Unlock()
			return
		}
		wg.Add(1)
		go func(tcData ToolCallEventData) {
			defer wg.Done()
			result := ms.dispatchToolCall(ctx, tcData)
			resMu.Lock()
			results = append(results, toolRes{TC: tcData, Result: result})
			resMu.Unlock()
		}(tc)
	}
	wg.Wait()

	var tcData []interface{}
	for _, r := range results {
		tcData = append(tcData, map[string]interface{}{
			"id":   r.TC.CallID,
			"type": "function",
			"function": map[string]interface{}{
				"name":      r.TC.Name,
				"arguments": r.TC.Arguments,
			},
		})
		ms.emit(ToolResult, map[string]interface{}{"tool_call": r.TC, "result": r.Result})
	}
	ms.session.AddMessageRaw(Message{
		Role:      "assistant",
		ToolCalls: tcData,
	})
	for _, r := range results {
		resultContent := strings.TrimSpace(r.Result)
		if resultContent == "" {
			resultContent = `{"result": "no result"}`
		} else if !strings.HasPrefix(resultContent, "{") && !strings.HasPrefix(resultContent, "[") {
			resultContent = fmt.Sprintf(`{"result": %s}`, jsonQuote(resultContent))
		}
		ms.session.AddMessageRaw(Message{
			Role:       "tool",
			Content:    resultContent,
			ToolCallID: r.TC.CallID,
			Name:       r.TC.Name,
		})
	}

	final, err := ms.orch.GetLLMProvider().Complete(ctx, ms.session.GetContextCopy(), ms.session.GetTools())
	ms.mu.Lock()
	if ms.state != StateInterrupted {
		ms.state = StateIdle
	}
	ms.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error after tool calls: %v", err))
		}
		return
	}
	if _, isMarker := parseToolCallMarker(final); isMarker {
		return
	}
	text := strings.TrimSpace(final)
	if text == "" {
		text = "Got it."
	}
	ms.session.AddMessage("assistant", text)
	ms.emit(BotResponse, text)
	ms.cacheResponse(userTranscript, text, nil)
	ms.speakText(ctx, text, gen)
}

func (ms *ManagedStream) runStreamingLLM(ctx context.Context, provider StreamingLLMProvider, gen int, userTranscript string) {
	var fullText strings.Builder
	var hasToolCalls bool
	messages := ms.session.GetContextCopy()

	type toolRes struct {
		TC     ToolCallEventData `json:"tool_call"`
		Result string            `json:"result"`
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

	// Soft-timeout filler: if the LLM hasn't produced a first token within ~3s,
	// speak a short filler to avoid dead air (ElevenLabs pattern). Fires once.
	firstToken := make(chan struct{})
	var fillerSpoken atomic.Bool
	go func() {
		select {
		case <-firstToken:
			return
		case <-time.After(3 * time.Second):
			if fillerSpoken.CompareAndSwap(false, true) {
				ms.speakText(ctx, "Hmm, let me think about that for a second.", gen)
			}
		case <-ctx.Done():
			return
		}
	}()

	_, err := provider.StreamComplete(ctx, messages, ms.session.GetTools(),
		func(chunk string) error {
			fullText.WriteString(chunk)
			pendingSentence.WriteString(chunk)

			// Signal first token arrival (stops the filler timer)
			select {
			case firstToken <- struct{}{}:
			default:
			}

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
			} else {
				// No pending text — speak a deterministic filler so there's no dead air
				// while the tool executes (Vapi/Pipecat pattern: platform speaks the
				// acknowledgment, not the LLM).
				fillerPhrase := toolFillerForLang(ms.session.GetCurrentLanguage())
				if fillerPhrase != "" {
					go func(t string) {
						sCtx, sCancel := context.WithCancel(ctx)
						defer sCancel()
						ms.speakText(sCtx, t, gen)
					}(fillerPhrase)
				}
			}

			toolWg.Add(1)
			go func(tcData ToolCallEventData) {
				defer toolWg.Done()
				result := ms.dispatchToolCall(ctx, tcData)
				toolMu.Lock()
				toolResults = append(toolResults, toolRes{TC: tcData, Result: result})
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
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
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
		ms.cacheResponse(userTranscript, response, nil)
	}

	if hasToolCalls {
		var tcData []interface{}
		for _, tr := range toolResults {
			tcData = append(tcData, map[string]interface{}{
				"id":   tr.TC.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tr.TC.Name,
					"arguments": tr.TC.Arguments,
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
			// Handle empty/malformed results: give the LLM something actionable
			// rather than an empty tool message it can't use.
			resultContent := strings.TrimSpace(tr.Result)
			if resultContent == "" {
				resultContent = `{"result": "no result"}`
			} else if !strings.HasPrefix(resultContent, "{") && !strings.HasPrefix(resultContent, "[") {
				// Wrap non-JSON results so the LLM can parse them consistently
				resultContent = fmt.Sprintf(`{"result": %s}`, jsonQuote(resultContent))
			}
			ms.session.AddMessageRaw(Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tr.TC.CallID,
				Name:       tr.TC.Name,
			})
		}

		go func() {
			freshCtx, c := context.WithCancel(ms.ctx)
			defer c()

			rCtx, rCancel := context.WithCancel(freshCtx)
			defer rCancel()

			ms.mu.Lock()
			if ms.pipelineCancel != nil {
				ms.pipelineCancel()
			}
			ms.pipelineCancel = rCancel
			ms.payloadGen++
			gen := ms.payloadGen
			ms.mu.Unlock()

			ms.emitWithGen(BotThinking, nil, gen)

			// Pass tools so the LLM can make further tool calls (multi-step chains).
			// Use streaming if available so we get real-time text + tool callbacks.
			tools := ms.session.GetTools()
			responseText := ""
			if sProv, ok := ms.orch.llm.(StreamingLLMProvider); ok && len(tools) > 0 {
				responseText, err = sProv.StreamComplete(rCtx, ms.session.GetContextCopy(), tools,
					func(chunk string) error { return nil }, // text handled below
					func(tc ToolCallEventData) error {
						// Multi-step tool chain: execute and append result to context.
						// Uses the same dispatchToolCall as round one (server handler
						// with a 15s timeout, or a client-side wait with a 10s
						// timeout) — previously this block had no client-tool branch
						// at all and returned "unknown tool" for any tool without a
						// registered server handler, silently breaking client-side
						// tools specifically on chained (round 2+) calls.
						ms.emit(ToolCall, tc)
						if !ms.session.RecordToolCall(tc.Name) {
							return fmt.Errorf("tool loop detected: %s", tc.Name)
						}
						res := ms.dispatchToolCall(rCtx, tc)
						ms.session.AddMessageRaw(Message{
							Role:       "tool",
							Content:    res,
							ToolCallID: tc.CallID,
							Name:       tc.Name,
						})
						ms.emit(ToolResult, map[string]interface{}{
							"tool_call": tc,
							"result":    res,
						})
						return nil
					})
				if err != nil {
					responseText = ""
				}
			} else {
				responseText, err = ms.orch.GetLLMProvider().Complete(rCtx, ms.session.GetContextCopy(), tools)
			}
			if err != nil {
				if rCtx.Err() == nil {
					ms.emit(ErrorEvent, fmt.Sprintf("LLM error after tool calls: %v", err))
				}
				ms.mu.Lock()
				if ms.state != StateInterrupted {
					ms.state = StateIdle
				}
				ms.mu.Unlock()
				return
			}
			// If the response is a tool-call marker, skip speaking it (tool results
			// are handled by the chain above).
			if strings.HasPrefix(responseText, "[TOOL_CALL") {
				ms.mu.Lock()
				if ms.state != StateInterrupted {
					ms.state = StateIdle
				}
				ms.mu.Unlock()
				return
			}
			text := strings.TrimSpace(responseText)
			if text == "" {
				text = "Got it."
			}

			ms.session.AddMessage("assistant", text)
			ms.emit(BotResponse, text)
			ms.speakText(rCtx, text, gen)
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

	messages := ms.session.GetContextCopy()

	// Trigger on message count OR estimated token count (research best practice:
	// token-based triggers catch long single messages that message-count misses).
	const maxContextTokens = 8000
	var totalTokens int
	for _, msg := range messages {
		totalTokens += estimateTokens(msg.Content)
	}
	overTokenBudget := totalTokens > maxContextTokens

	if !ms.session.NeedsSummarization() && !overTokenBudget {
		return
	}

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
	if overTokenBudget {
		// If over token budget, summarize more aggressively
		summarizeCount = int(float64(len(turnsToSummarize)) * 0.6)
		if summarizeCount < 2 {
			summarizeCount = 2
		}
	}
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

// estimateTokens approximates token count for a string (~4 chars/token on average
// for English; a reasonable proxy across languages).
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

func (ms *ManagedStream) SetClientVAD(enabled bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.clientVAD = enabled
	ms.logger.Info("Client VAD mode", "enabled", enabled)
}

// toolFillerForLang returns a short, deterministic verbal acknowledgment to speak
// while a tool is executing. This avoids dead air without an LLM round-trip.
func toolFillerForLang(lang Language) string {
	switch lang {
	case LanguageEs:
		return "Un momento, déjame buscarlo."
	case LanguageFr:
		return "Un instant, je vérifie ça."
	case LanguageDe:
		return "Einen Moment, ich schaue das nach."
	case LanguageIt:
		return "Un momento, lo controllo."
	case LanguagePt:
		return "Um momento, deixa eu verificar."
	default:
		return "Let me look that up for you."
	}
}

// jsonQuote safely quotes a string for inclusion in a JSON value, escaping any
// quotes, backslashes, and control characters so the resulting JSON is valid.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (ms *ManagedStream) IsClientVAD() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.clientVAD
}
