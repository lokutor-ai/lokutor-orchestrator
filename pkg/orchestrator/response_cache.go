package orchestrator

import (
	"sync"
	"time"
)

type CachedResponse struct {
	Response string
	Audio    []byte
	Expires  time.Time
}

type ResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*CachedResponse
	ttl     time.Duration
	maxSize int
}

func NewResponseCache(ttl time.Duration, maxSize int) *ResponseCache {
	c := &ResponseCache{
		entries: make(map[string]*CachedResponse),
		ttl:     ttl,
		maxSize: maxSize,
	}
	go c.evictLoop()
	return c
}

func (c *ResponseCache) evictLoop() {
	for {
		time.Sleep(30 * time.Second)
		c.mu.Lock()
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.Expires) {
				delete(c.entries, k)
			}
		}
		c.mu.Unlock()
	}
}

func (c *ResponseCache) Get(key string) (string, []byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.Expires) {
		return "", nil, false
	}
	return entry.Response, entry.Audio, true
}

func (c *ResponseCache) Set(key, response string, audio []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxSize {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[key] = &CachedResponse{
		Response: response,
		Audio:    audio,
		Expires:  time.Now().Add(ttl),
	}
}

func (c *ResponseCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *ResponseCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CachedResponse)
}

func CacheKeyFor(text, lastUserText string) string {
	if text == "[USER_SILENCE_TIMEOUT]" {
		return "silence_timeout"
	}
	if len(text) > 64 {
		text = text[:64]
	}
	return "q:" + text + "|last:" + lastUserText
}
