package llm

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// sharedLLMClient is a process-wide HTTP client reused by all LLM providers.
// Go's http.DefaultClient has only 2 idle connections per host — under
// concurrent voice-agent load this creates connection churn (TCP+TLS handshakes
// every few requests), adding ~100-150ms per LLM call on high-latency links
// like eu-west-1 → US inference clusters.
//
// A persistent client with a larger connection pool eliminates handshake
// overhead for subsequent requests and HTTP/2 multiplexing eliminates
// head-of-line blocking across concurrent streams on the same TCP
// connection.
var (
	sharedLLMOnce   sync.Once
	sharedLLMClient *http.Client
)

func getSharedLLMClient() *http.Client {
	sharedLLMOnce.Do(func() {
		sharedLLMClient = &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				// Connection pool: allow enough idle connections for
				// concurrent voice-agent sessions (8+ concurrent LLM calls).
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     60 * time.Second,

				// Dial/TLS timeouts prevent hanging on unreachable hosts.
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,

				// HTTP/2 is enabled by Go's default transport when the
				// server supports ALPN TLS — which Cerebras, Groq, and
				// Gemini all do. No explicit config needed.
			},
		}
	})
	return sharedLLMClient
}
