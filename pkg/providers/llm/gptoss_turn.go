package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

// oneTurnFor cuts a gpt-oss reply to the turn it was asked for (orchestrator.ReplyTurn), and passes
// any other model's reply through: nil passes everything.
func oneTurnFor(ctx context.Context, model string) *orchestrator.ReplyTurn {
	if !strings.Contains(strings.ToLower(model), "gpt-oss") {
		return nil
	}
	return orchestrator.NewReplyTurn(ctx)
}

// drainTimeout bounds how long a cut reply's stream is read on, and so how long ChainLLM keeps the
// winning attempt's context alive after it returns.
const drainTimeout = 15 * time.Second

// drainAfterCut reads the rest of a stream the reply was cut from, in the background, so the turn
// is not held until the model stops talking to itself: the tokens are still counted, and what was
// dropped is kept for the log. The body is closed when the stream ends, ctx ends, or at drainTimeout.
func drainAfterCut(ctx context.Context, reader *bufio.Reader, body io.Closer, cut *orchestrator.RunOnCut) {
	done := orchestrator.TokenUsageFrom(ctx).DrainStarted()
	go func() {
		defer done()
		timer := time.AfterFunc(drainTimeout, func() { body.Close() })
		defer timer.Stop()
		defer body.Close()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}
			recordUsageFromChunk(ctx, []byte(data))
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
				cut.Drop(chunk.Choices[0].Delta.Content)
			}
		}
	}()
}
