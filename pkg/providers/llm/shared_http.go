package llm

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
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

// Reasoning models emit their entire chain of thought before the first token
// a voice agent can actually speak, and that silence lands on the caller.
// Measured from the production worker against openai/gpt-oss-120b with a
// realistic agent prompt: 24-66 reasoning deltas before the first content
// token, median 643ms at the API default versus 306ms at "low" — for the same
// answer, word for word. Both Groq and Cerebras serve gpt-oss and both accept
// the parameter, so the default lives here rather than in either provider.
//
// Only the gpt-oss family takes it; the APIs answer 400 for models that don't,
// so this stays keyed on the model name rather than sent blindly.
// GROQ_REASONING_EFFORT overrides it ("low"/"medium"/"high", or "default"/"off"
// to send nothing) without a rebuild, so a turn-quality regression is one env
// change away from being reverted.
func defaultReasoningEffort(model string) string {
	if v := strings.TrimSpace(os.Getenv("GROQ_REASONING_EFFORT")); v != "" {
		if strings.EqualFold(v, "default") || strings.EqualFold(v, "off") {
			return ""
		}
		return strings.ToLower(v)
	}
	if strings.Contains(strings.ToLower(model), "gpt-oss") {
		return "low"
	}
	return ""
}

// setReasoningEffort adds the parameter only when one is configured. An
// explicit null or empty string is a 400 just as surely as the wrong model, so
// the key must be absent rather than present-and-empty.
func setReasoningEffort(payload map[string]interface{}, effort string) {
	if effort != "" {
		payload["reasoning_effort"] = effort
	}
}

// WarmupHost opens (and pools, via keep-alive) a TCP+TLS connection to host
// through the shared LLM client, without making a real API call. The very
// first genuine LLM request of a freshly-started process otherwise pays
// that handshake — typically ~100-150ms on a cross-region link — inline
// with a real caller's latency, since getSharedLLMClient's pool starts
// empty. A bare GET to the provider's base URL gets rejected (404/401/etc)
// almost immediately, but that's fine: completing the handshake and
// leaving the connection idle in the pool is the only thing this is for,
// so the response (and any error past the handshake itself) is ignored.
// Meant to be called once, in the background, as early as possible at
// process startup — not per-call.
func WarmupHost(ctx context.Context, host string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host, nil)
	if err != nil {
		return
	}
	resp, err := getSharedLLMClient().Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
