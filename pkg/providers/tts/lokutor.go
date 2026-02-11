package tts

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type LokutorTTS struct {
	apiKey string
	host   string
	scheme string
	mu     sync.Mutex
	conn   *websocket.Conn
}

func NewLokutorTTS(apiKey string) *LokutorTTS {
	return &LokutorTTS{
		apiKey: apiKey,
		host:   "api.lokutor.com",
		scheme: "wss",
	}
}

func (t *LokutorTTS) getConn(ctx context.Context) (*websocket.Conn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		return t.conn, nil
	}

	u := url.URL{Scheme: t.scheme, Host: t.host, Path: "/ws", RawQuery: "api_key=" + t.apiKey}
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to lokutor: %w", err)
	}

	// Increase read limit to 10MB to handle large audio chunks
	conn.SetReadLimit(10 * 1024 * 1024)

	t.conn = conn
	return conn, nil
}

func (t *LokutorTTS) Synthesize(ctx context.Context, text string, voice orchestrator.Voice, lang orchestrator.Language) ([]byte, error) {
	var audio []byte
	err := t.StreamSynthesize(ctx, text, voice, lang, func(chunk []byte) error {
		audio = append(audio, chunk...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return audio, nil
}

func (t *LokutorTTS) StreamSynthesize(ctx context.Context, text string, voice orchestrator.Voice, lang orchestrator.Language, onChunk func([]byte) error) error {
	conn, err := t.getConn(ctx)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	req := map[string]interface{}{
		"text":    text,
		"voice":   string(voice),
		"lang":    string(lang),
		"speed":   1.0,
		"steps":   6, // Matched to your flawless Python script
		"visemes": false,
	}

	if err := wsjson.Write(ctx, conn, req); err != nil {
		t.conn = nil
		conn.Close(websocket.StatusAbnormalClosure, "failed to write json")
		return fmt.Errorf("failed to send synthesis request: %w", err)
	}

	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			t.conn = nil
			conn.Close(websocket.StatusAbnormalClosure, "failed to read")
			return fmt.Errorf("failed to read from lokutor: %w", err)
		}

		switch messageType {
		case websocket.MessageBinary:
			// ZERO-LATENCY PASSTHROUGH:
			// Send raw payload immediately to avoid any "chunking" or delays between segments.
			if err := onChunk(payload); err != nil {
				return err
			}
		case websocket.MessageText:
			msg := string(payload)
			if msg == "EOS" {
				return nil
			}
			if len(msg) >= 4 && msg[:4] == "ERR:" {
				return fmt.Errorf("lokutor error: %s", msg)
			}
		}
	}
}

func (t *LokutorTTS) Name() string {
	return "lokutor"
}

func (t *LokutorTTS) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != nil {
		err := t.conn.Close(websocket.StatusNormalClosure, "")
		t.conn = nil
		return err
	}
	return nil
}
