package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
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
//
// Every call's outcome is logged ("Tool call", or "Tool call failed" with the error). A failed tool
// used to reach the model and nothing else: handlers report failure as an {"error": ...} result, so a
// calendar that could not be reached on every booking left no trace until a caller noticed nothing
// had been booked (2026-09-30).
func (ms *ManagedStream) dispatchToolCall(ctx context.Context, tcData ToolCallEventData) string {
	start := time.Now()
	res := ms.runToolCall(ctx, tcData)
	ms.mu.Lock()
	gen := ms.payloadGen
	ms.mu.Unlock()
	switch msg := toolResultError(res); {
	case msg == "":
		ms.logger.Info("Tool call", "tool", tcData.Name, "ms", time.Since(start).Milliseconds(), "gen", gen)
	case msg == "cancelled" && ms.ctx.Err() != nil:
		ms.logger.Info("Tool call cancelled: the call ended", "tool", tcData.Name, "ms", time.Since(start).Milliseconds())
	case msg == "cancelled":
		ms.logger.Info("Tool call cancelled: the turn was superseded", "tool", tcData.Name, "ms", time.Since(start).Milliseconds(), "gen", gen)
	default:
		ms.logger.Warn("Tool call failed", "tool", tcData.Name, "ms", time.Since(start).Milliseconds(), "gen", gen,
			"error", clipForLog(msg))
	}
	return res
}

// toolResultError is the error a tool result reports: its top-level "error" field, if it is a JSON
// object with a non-empty one.
func toolResultError(res string) string {
	s := strings.TrimSpace(res)
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	switch raw := strings.TrimSpace(string(v.Error)); raw {
	case "", "null", `""`, "false":
		return ""
	default:
		var msg string
		if json.Unmarshal(v.Error, &msg) == nil {
			return msg
		}
		return raw
	}
}

func (ms *ManagedStream) runToolCall(ctx context.Context, tcData ToolCallEventData) string {
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
		if ctx.Err() != nil {
			// resultCtx is a child of the turn's context: it also ends when the caller speaks over
			// the agent and the turn is superseded, after a few hundred milliseconds. That is a
			// cancellation, not a client that never answered; it was reported as a 10-second
			// timeout and counted by the tool-calls-failing alarm (five barge-ins in an hour).
			return `{"error": "cancelled"}`
		}
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
// which LLM provider is configured. Loops (bounded by maxNonStreamingToolRounds)
// when the follow-up answer is itself another marker, so multi-step tool
// chains (e.g. look up availability, then book it) work the same way on
// Anthropic/OpenAI as they already do on the streaming providers — a single
// round used to be a hard ceiling here: the second round's marker was
// silently discarded and the caller got no response at all for that turn.
// maxNonStreamingToolRounds bounds how many back-to-back [TOOL_CALLS] rounds
// handleNonStreamingToolCalls will chain before giving up. The per-tool-name
// cap in ConversationSession.RecordToolCall (3 calls/tool/session) already
// blocks the common infinite-loop case; this is a coarser backstop against a
// pathological chain across many distinct tool names.
const maxNonStreamingToolRounds = 8

// errUnspeakableReply stops a reply stream at a sentence that must not be spoken (unspeakableReply).
var errUnspeakableReply = errors.New("reply is not speech")

// maxChainedToolRounds is how many rounds of tool calls the model may make after the first one's
// results, on the streaming path, before it has to answer with what it has.
const maxChainedToolRounds = 4

func (ms *ManagedStream) handleNonStreamingToolCalls(ctx context.Context, gen int, userTranscript string, calls []ToolCallEventData) {
	if fillerPhrase := ms.toolFiller(); fillerPhrase != "" {
		go func(t string) {
			sCtx, sCancel := context.WithCancel(ctx)
			defer sCancel()
			ms.speakText(sCtx, t, gen)
		}(fillerPhrase)
	}

	setIdle := func() {
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
	}

	for round := 0; round < maxNonStreamingToolRounds; round++ {
		var results []toolExchange
		var resMu sync.Mutex
		var wg sync.WaitGroup

		for _, tc := range calls {
			ms.emit(ToolCall, tc)
			if !ms.session.RecordToolCall(tc.Name) {
				ms.emit(ErrorEvent, fmt.Sprintf("Tool loop detected: %s called too many times. Aborting to prevent infinite retry.", tc.Name))
				setIdle()
				return
			}
			wg.Add(1)
			go func(tcData ToolCallEventData) {
				defer wg.Done()
				result := ms.dispatchToolCall(ctx, tcData)
				resMu.Lock()
				results = append(results, toolExchange{TC: tcData, Result: result})
				resMu.Unlock()
			}(tc)
		}
		wg.Wait()

		for _, r := range results {
			ms.emit(ToolResult, map[string]interface{}{"tool_call": r.TC, "result": r.Result})
		}
		ms.recordToolExchange("", results)

		final, err := ms.orch.GetLLMProvider().Complete(ctx, ms.session.GetContextCopy(), ms.session.GetTools())
		if err != nil {
			setIdle()
			if ctx.Err() == nil {
				ms.emit(ErrorEvent, fmt.Sprintf("LLM error after tool calls: %v", err))
			}
			return
		}
		if nextCalls, isMarker := parseToolCallMarker(final); isMarker {
			calls = nextCalls
			continue
		}

		setIdle()
		text := strings.TrimSpace(final)
		if text == "" {
			text = "Got it."
		}
		ms.session.AddMessage("assistant", text)
		ms.emitBotResponse(text)
		ms.cacheResponse(userTranscript, text, nil)
		ms.speakResponse(ctx, text, gen)
		return
	}

	ms.emit(ErrorEvent, "Tool call chain exceeded max rounds without a final answer")
	setIdle()
}

func (ms *ManagedStream) runStreamingLLM(ctx context.Context, provider StreamingLLMProvider, gen int, userTranscript string) {
	// Per-turn reset, mirroring runLLMAndTTS's non-streaming path: these three
	// fields drive truncateSpokenContext's "was anything actually spoken"
	// decision on interrupt and must not carry a stale value across turns
	// (see the reset in runLLMAndTTS for the bug this previously caused —
	// this path never reset them at all, so spokenTextLocked in particular
	// latched true on turn one's first audio chunk and then stayed true for
	// the rest of the session, freezing spokenTextPrefix to that first turn's
	// text).
	// A fresh token sink per turn, installed on the context the provider will use. Tool chains
	// make several provider calls inside one turn and TokenUsage accumulates across them, so what
	// the turn-latency line reports is the whole turn's cost rather than the last call's.
	turnTokens := &TokenUsage{}
	ctx = WithTokenUsage(ctx, turnTokens)

	ms.mu.Lock()
	ms.spokenTextPrefix = ""
	ms.spokenTextLocked = false
	ms.responseChunksSent = 0
	ms.turnTokens = turnTokens
	ms.mu.Unlock()

	var fullText strings.Builder
	var hasToolCalls bool
	// Plus the check-in note when this is the silence nudge — see llmMessages.
	messages := ms.llmMessages(userTranscript)

	var toolResults []toolExchange
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
	// The FIRST chunk of a response is flushed to TTS eagerly (see the flush
	// loop below) so the bot starts talking sooner: TTS time-to-first-audio
	// scales with chunk length, so a long opening sentence otherwise makes the
	// caller wait for the whole thing to synthesize before hearing anything.
	// Once the first chunk is out, later chunks flush on sentence boundaries
	// for natural prosody. Safe because the first chunk's playback time covers
	// the next chunk's synthesis, so audio stays gapless.
	// Splitting the opening chunk is OFF by default.
	//
	// It used to cut the first chunk at a clause boundary, or failing that at
	// whatever word fell before 32 characters. That buys a faster first byte
	// and pays for it with the thing people actually notice: the agent said
	// about five words, stopped dead, and resumed. Prosody is generated per
	// synthesis call, so a sentence cut in half is spoken as two sentences —
	// wrong intonation on both halves and a seam in the middle. A slightly
	// later start that sounds like a person beats an immediate start that
	// sounds broken.
	//
	// Set TTS_FIRST_CHUNK_MAX_CHARS to a positive value to bring the old
	// behaviour back; the value is the hard cap on the opening chunk.
	firstChunkDone := false
	const firstChunkClauseMin = 12 // don't flush a clause shorter than this
	firstChunkMaxChars := 0        // 0 = never split the first chunk early
	if v := os.Getenv("TTS_FIRST_CHUNK_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= firstChunkClauseMin {
			firstChunkMaxChars = n
		}
	}
	splitFirstChunk := firstChunkMaxChars > 0

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
				// In the call's language: this was English on every call, so a Spanish caller
				// waiting on a slow model heard "Hmm, let me think about that for a second."
				ms.speakText(ctx, thinkingFillerForLang(ms.session.GetCurrentLanguage()), gen)
			}
		case <-ctx.Done():
			return
		}
	}()

	// unspeakable is set when the reply turns out to be a tool call written out, or the model's
	// reasoning, instead of speech (unspeakableReply). The stream is stopped at that sentence, before
	// it is spoken, and the model asked once more: the call it meant to make was never made, and a
	// second try usually makes it. On 2026-09-29 ten of a hundred benchmark calls hit this, mostly
	// "add it to my cart", and until the speech guard callers heard "product id prod quantity one".
	tools := ms.session.GetTools()
	unspeakable := ""
	var err error
	for attempt := 1; ; attempt++ {
		unspeakable = ""
		_, err = provider.StreamComplete(ctx, messages, ms.toolsOffered(userTranscript),
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

				// Decide where to flush. Sentence-ending punctuation always flushes
				// (natural prosody). For the FIRST chunk only, also flush at a clause
				// boundary (comma/semicolon/colon) past a small minimum, and if no
				// punctuation has appeared by firstChunkMaxChars, cut at the last
				// word boundary — so the opening chunk stays short and the bot
				// starts speaking quickly instead of waiting for a whole long
				// sentence to synthesize.
				// Sentence-ending punctuation is the only boundary the synthesiser can be handed
				// without changing how the line is spoken — but only when it really ends a sentence.
				// Cutting at every '.' turned "a las 4 p.m." into three synthesis calls and "3.14"
				// into two, each paying the engine's start-up cost again (an audible gap) and each
				// too short for the language token to condition anything (an English-sounding
				// fragment mid-sentence). See sentence_boundary.go.
				flushEnd := nextFlushPoint(
					buf, false,
					splitFirstChunk && !firstChunkDone, firstChunkClauseMin,
					minSpokenSegment,
				)
				if splitFirstChunk && flushEnd < 0 && !firstChunkDone && len(buf) >= firstChunkMaxChars {
					if sp := strings.LastIndexByte(strings.TrimRight(buf[:firstChunkMaxChars], " "), ' '); sp > firstChunkClauseMin {
						flushEnd = sp + 1
					}
				}
				if flushEnd > 0 {
					seg := strings.TrimSpace(buf[:flushEnd])
					if reason := unspeakableReply(seg, tools); reason != "" && !hasToolCalls {
						unspeakable = reason
						return errUnspeakableReply
					}
					if seg != "" {
						ttsQueue <- seg
						firstChunkDone = true
					}
					rest := strings.TrimSpace(buf[flushEnd:])
					pendingSentence.Reset()
					pendingSentence.WriteString(rest)
				}

				return nil
			},
			func(tc ToolCallEventData) error {
				// Check for infinite tool loop
				if !ms.session.RecordToolCall(tc.Name) {
					ms.emit(ErrorEvent, fmt.Sprintf("Tool loop detected: %s called too many times. Aborting to prevent infinite retry.", tc.Name))
					return fmt.Errorf("tool loop detected: %s", tc.Name)
				}

				firstCall := !hasToolCalls
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
				} else if firstCall {
					// No pending text — speak a filler so there's no dead air while the tool
					// executes (Vapi/Pipecat pattern: platform speaks the acknowledgment, not
					// the LLM). Once per round: parallel calls in one round used to each speak
					// one, back to back.
					fillerPhrase := ms.toolFiller()
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
					toolResults = append(toolResults, toolExchange{TC: tcData, Result: result})
					toolMu.Unlock()
				}(tc)

				return nil
			},
		)
		if err == nil && unspeakable == "" && !hasToolCalls {
			// The last sentence is only whole once the stream has ended.
			unspeakable = unspeakableReply(strings.TrimSpace(pendingSentence.String()), tools)
		}
		if unspeakable == "" || ctx.Err() != nil {
			break
		}
		ms.logger.Warn("Not speaking: the model wrote its tool call or its reasoning as the reply",
			"reason", unspeakable, "attempt", attempt, "gen", gen, "text", fullText.String())
		fullText.Reset()
		pendingSentence.Reset()
		if attempt == 2 {
			break
		}
	}
	if errors.Is(err, errUnspeakableReply) {
		err = nil
	}

	toolWg.Wait()

	flushSentence()
	close(ttsQueue)
	ttsWg.Wait()
	ms.logRunOns(gen, turnTokens)

	if unspeakable != "" && !hasToolCalls && err == nil {
		// Both tries came back unspeakable: the caller hears nothing for this turn, and says so here.
		ms.logger.Warn("Turn abandoned: the model's reply was not speech twice in a row", "reason", unspeakable, "gen", gen)
		ms.mu.Lock()
		if ms.state != StateInterrupted {
			ms.state = StateIdle
		}
		ms.mu.Unlock()
		return
	}

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

	if !hasToolCalls {
		// If the turn was interrupted before StreamComplete unblocked (e.g. in the gap between two
		// sentences of a multi-sentence reply), fullText can still hold text streamed in after the
		// interrupt, since the provider callback isn't itself ctx-aware — so the full response must
		// not be recorded as delivered. handleInterrupt records what the caller actually heard
		// (spoken_truth.go) and can race ahead of this point; commitStreamedReply is ordered against
		// it so exactly one of them writes the reply.
		ms.commitStreamedReply(ctx, gen, response, userTranscript)
	}

	if hasToolCalls {
		for _, tr := range toolResults {
			ms.emit(ToolResult, tr)
		}
		ms.recordToolExchange(response, toolResults)

		go func() {
			freshCtx, c := context.WithCancel(ms.ctx)
			defer c()

			rCtx, rCancel := context.WithCancel(freshCtx)
			defer rCancel()
			// The turn's sink, so these rounds' tokens count toward the turn and a cut reply is noted.
			rCtx = WithTokenUsage(rCtx, turnTokens)
			// The turn-latency line is written at the turn's first audio, which in a tool turn is the
			// filler, before these rounds run: their tokens were in no log line (2026-10-01 audit). This
			// line carries the whole turn's total once they are done.
			defer whenTokensSettled(turnTokens, func() {
				p, c, t, ok := turnTokens.Snapshot()
				if !ok {
					p, c, t = -1, -1, -1
				}
				ms.logger.Info("Turn tokens after tools", "gen", gen, "prompt_tokens", p, "completion_tokens", c,
					"total_tokens", t, "answered_by", turnTokens.AnsweredBy())
			})

			ms.mu.Lock()
			// A newer turn has the floor: the caller spoke again while the tools ran. The exchange is
			// in the context already (recordToolExchange above), so the newer turn's reply can say
			// what was done; this one must not be spoken too, nor cancel the newer turn's pipeline as
			// it takes the floor. On 2026-10-01 a booking made for "la parejo" finished after the
			// caller's "sí, venga" had started its own turn, and both turns' replies were spoken
			// ("Genial, te dejo la demo…", then "Listo, te confirmo la cita…").
			if ms.payloadGen != gen {
				current := ms.payloadGen
				ms.mu.Unlock()
				ms.logger.Info("Reply after tool calls not spoken: a newer turn has the floor",
					"gen", gen, "current_gen", current)
				return
			}
			if ms.pipelineCancel != nil {
				ms.pipelineCancel()
			}
			ms.pipelineCancel = rCancel
			ms.pipelineCtx = rCtx
			ms.payloadGen++
			gen := ms.payloadGen
			ms.mu.Unlock()

			// BotThinking is emitted from inside speakText (via speakResponse at the end of this
			// goroutine), not here — see the comment on that emission for why.

			// Pass tools so the model can chain further calls. Each round's calls are recorded with
			// their results (recordToolExchange) and the model is asked again, until it answers in
			// words or maxChainedToolRounds runs out. Round two used to record only the results — a
			// tool message answering no call, which Cerebras rejects with a 400, so every later turn
			// of the call failed too and the caller heard nothing for the rest of it — and never asked
			// again, so a chained call was answered "Got it." (Full-Duplex-Bench v3, 2026-09-29).
			tools := ms.session.GetTools()
			responseText := ""
			sProv, streaming := ms.orch.llm.(StreamingLLMProvider)
			for round := 1; ; round++ {
				if !streaming || len(tools) == 0 {
					responseText, err = ms.orch.GetLLMProvider().Complete(rCtx, ms.session.GetContextCopy(), tools)
					break
				}
				var calls []toolExchange
				responseText, err = sProv.StreamComplete(rCtx, ms.session.GetContextCopy(), tools,
					func(chunk string) error { return nil }, // text handled below
					func(tc ToolCallEventData) error {
						// The same dispatchToolCall as round one: a server handler (15 s timeout) or
						// the client's answer (10 s). Past the per-tool cap the model is told so,
						// rather than the turn failing: an error here would leave the caller silent.
						ms.emit(ToolCall, tc)
						res := `{"error": "this tool has already been called three times for this request; answer the caller with what you have"}`
						if ms.session.RecordToolCall(tc.Name) {
							res = ms.dispatchToolCall(rCtx, tc)
						}
						calls = append(calls, toolExchange{TC: tc, Result: res})
						ms.emit(ToolResult, map[string]interface{}{
							"tool_call": tc,
							"result":    res,
						})
						return nil
					})
				ms.recordToolExchange("", calls)
				if err != nil {
					responseText = ""
					break
				}
				if len(calls) == 0 || round >= maxChainedToolRounds {
					break
				}
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
			// The reply after a tool call is spoken whole, so it gets the check round one gives each
			// sentence (unspeakableReply): this path had none, and a tool call written out here, or
			// the model's reasoning, went straight to the caller.
			if reason := unspeakableReply(text, tools); reason != "" {
				ms.logger.Warn("Not speaking after tool calls: the model wrote a tool call or its reasoning as the reply",
					"reason", reason, "gen", gen, "text", text)
				retry, rerr := ms.orch.GetLLMProvider().Complete(rCtx, ms.session.GetContextCopy(), tools)
				retry, _ = OneTurn(rCtx, retry)
				if rerr != nil || unspeakableReply(retry, tools) != "" || strings.TrimSpace(retry) == "" {
					ms.logger.Warn("Turn abandoned after tool calls: the model's reply was not speech twice in a row",
						"reason", reason, "gen", gen, "retry", retry, "error", rerr)
					ms.logRunOns(gen, turnTokens)
					ms.mu.Lock()
					if ms.state != StateInterrupted {
						ms.state = StateIdle
					}
					ms.mu.Unlock()
					return
				}
				text = strings.TrimSpace(retry)
			}
			ms.logRunOns(gen, turnTokens)
			if text == "" {
				text = "Got it."
			}

			ms.session.AddMessage("assistant", text)
			ms.emitBotResponse(text)
			ms.speakResponse(rCtx, text, gen)
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

// thinkingFillerForLang is what the agent says when the model has produced nothing after three
// seconds, so the caller does not sit in silence. (The line spoken while a tool runs is
// ms.toolFiller, tool_filler.go.)
func thinkingFillerForLang(lang Language) string {
	switch lang {
	case LanguageEs:
		return "Mmm, déjame pensarlo un momento."
	case LanguageCa:
		return "Mmm, deixa'm pensar-ho un moment."
	case LanguageGl:
		return "Mmm, déixame pensalo un momento."
	case LanguageEu:
		return "Mmm, utzi pentsatzen une batez."
	case LanguageFr:
		return "Hmm, laissez-moi réfléchir un instant."
	case LanguageDe:
		return "Hmm, lassen Sie mich kurz nachdenken."
	case LanguageIt:
		return "Mmm, fammi pensare un attimo."
	case LanguagePt:
		return "Hmm, deixa-me pensar um momento."
	default:
		return "Hmm, let me think about that for a second."
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
