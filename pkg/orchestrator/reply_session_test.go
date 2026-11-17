package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

// replyTTS records each sentence's reply id and the replies ended.
type replyTTS struct {
	countingTTS
	rmu   sync.Mutex
	ids   []string
	ended []string
}

func (r *replyTTS) StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error {
	r.rmu.Lock()
	r.ids = append(r.ids, ReplyID(ctx))
	r.rmu.Unlock()
	return onChunk(make([]byte, 4410))
}

func (r *replyTTS) EndReply(id string) {
	r.rmu.Lock()
	r.ended = append(r.ended, id)
	r.rmu.Unlock()
}

// Every sentence of a turn carries one reply id, a new turn a new one, and the provider hears each turn's end once, after
// its last sentence (the synthesis pool holds a slot for the next sentence until then).
func TestEveryTurnIsOneReply(t *testing.T) {
	llm := &MockStreamingLLM{responses: []struct {
		content   string
		toolCalls []ToolCallEventData
	}{
		{content: "Claro. Te lo miro ahora mismo. Un segundo."},
		{content: "Ya está. Mañana a las diez."},
	}}
	tts := &replyTTS{}
	orch := NewWithAllLayers(&MockSTTProvider{transcribeResult: "x"}, llm, tts, nil, DefaultConfig(), &NoOpLogger{})
	ms := orch.NewManagedStream(context.Background(), NewConversationSession("reply-ids"))
	defer ms.Close()
	ms.runLLMAndTTS(context.Background(), "una cita")
	ms.runLLMAndTTS(context.Background(), "gracias")
	deadline := time.Now().Add(5 * time.Second)
	for {
		tts.rmu.Lock()
		ids, ended := append([]string(nil), tts.ids...), append([]string(nil), tts.ended...)
		tts.rmu.Unlock()
		if len(ended) >= 2 || time.Now().After(deadline) {
			if len(ended) != 2 || ended[0] == ended[1] || ended[0] == "" {
				t.Fatalf("expected two distinct ended replies, got %v (sentences %v)", ended, ids)
			}
			first, second := 0, 0
			for _, id := range ids {
				switch id {
				case ended[0]:
					first++
				case ended[1]:
					second++
				default:
					t.Fatalf("sentence with reply id %q outside both turns %v", id, ended)
				}
			}
			if first < 2 || second < 1 {
				t.Fatalf("expected each turn's sentences under its own id: %v vs ended %v", ids, ended)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
