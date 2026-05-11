package tts

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type poolConn struct {
	conn      *websocket.Conn
	inUse     bool
	createdAt time.Time
}

type LokutorTTS struct {
	apiKey     string
	host       string
	scheme     string
	mu         sync.Mutex
	pool       []*poolConn
	poolSize   int
	maxConnAge time.Duration
}

func NewLokutorTTS(apiKey string) *LokutorTTS {
	return &LokutorTTS{
		apiKey:     apiKey,
		host:       "api.lokutor.com",
		scheme:     "wss",
		poolSize:   1,
		maxConnAge: 25 * time.Second,
	}
}

func NewLokutorTTSPool(apiKey string, poolSize int) *LokutorTTS {
	if poolSize < 1 {
		poolSize = 1
	}
	tts := &LokutorTTS{
		apiKey:     apiKey,
		host:       "api.lokutor.com",
		scheme:     "wss",
		poolSize:   poolSize,
		pool:       make([]*poolConn, 0, poolSize),
		maxConnAge: 25 * time.Second,
	}
	tts.warmup()
	return tts
}

func (t *LokutorTTS) warmup() {
	for i := 0; i < t.poolSize; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := t.dial(ctx)
		cancel()
		if err == nil {
			t.pool = append(t.pool, &poolConn{conn: conn, createdAt: time.Now()})
		}
	}
}

func (t *LokutorTTS) dial(ctx context.Context) (*websocket.Conn, error) {
	u := url.URL{Scheme: t.scheme, Host: t.host, Path: "/ws", RawQuery: "api_key=" + t.apiKey}
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to lokutor: %w", err)
	}
	conn.SetReadLimit(10 * 1024 * 1024)
	return conn, nil
}

func (t *LokutorTTS) acquire(ctx context.Context) (*websocket.Conn, error) {
	t.mu.Lock()

	for _, pc := range t.pool {
		if !pc.inUse {
			if time.Since(pc.createdAt) > t.maxConnAge {
				pc.conn.Close(websocket.StatusNormalClosure, "stale")
				pc.conn = nil
			}
			if pc.conn == nil {
				var err error
				pc.conn, err = t.dial(ctx)
				if err != nil {
					t.mu.Unlock()
					return nil, err
				}
				pc.createdAt = time.Now()
			}
			pc.inUse = true
			conn := pc.conn
			t.mu.Unlock()
			return conn, nil
		}
	}

	if len(t.pool) < t.poolSize {
		conn, err := t.dial(ctx)
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		pc := &poolConn{conn: conn, inUse: true, createdAt: time.Now()}
		t.pool = append(t.pool, pc)
		t.mu.Unlock()
		return conn, nil
	}

	t.mu.Unlock()

	conn, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (t *LokutorTTS) release(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, pc := range t.pool {
		if pc.conn == conn {
			pc.inUse = false
			return
		}
	}
}

func (t *LokutorTTS) evict(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	conn.Close(websocket.StatusAbnormalClosure, "evicted")
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, pc := range t.pool {
		if pc.conn == conn {
			t.pool = append(t.pool[:i], t.pool[i+1:]...)
			return
		}
	}
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
	conn, err := t.acquire(ctx)
	if err != nil {
		return err
	}

	req := map[string]interface{}{
		"text":    text,
		"voice":   string(voice),
		"lang":    string(lang),
		"speed":   1.0,
		"steps":   6,
		"visemes": false,
	}

	if err := wsjson.Write(ctx, conn, req); err != nil {
		t.evict(conn)
		return fmt.Errorf("failed to send synthesis request: %w", err)
	}

	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			t.evict(conn)
			return fmt.Errorf("failed to read from lokutor: %w", err)
		}

		switch messageType {
		case websocket.MessageBinary:
			chunk := make([]byte, len(payload))
			copy(chunk, payload)
			if err := onChunk(chunk); err != nil {
				t.evict(conn)
				return err
			}
		case websocket.MessageText:
			msg := string(payload)
			if msg == "EOS" {
				t.release(conn)
				return nil
			}
			if len(msg) >= 4 && msg[:4] == "ERR:" {
				t.evict(conn)
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
	for _, pc := range t.pool {
		if pc.conn != nil {
			pc.conn.Close(websocket.StatusNormalClosure, "")
		}
	}
	t.pool = nil
	return nil
}

func (t *LokutorTTS) Abort() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, pc := range t.pool {
		if pc.inUse && pc.conn != nil {
			pc.conn.Close(websocket.StatusAbnormalClosure, "abort")
			pc.conn = nil
			pc.inUse = false
		}
	}
	return nil
}
