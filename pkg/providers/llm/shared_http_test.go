package llm

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The shared LLM transport has its own DialContext, which keeps a Go Transport on HTTP/1.1 unless HTTP/2 is
// forced: production spoke HTTP/1.1 to every provider until 2026-10-16. Against a TLS server that offers h2 it
// must negotiate HTTP/2, and a stream cancelled half-way (a speculative guess the caller talked over) must leave
// the connection for the requests after it instead of costing each a new handshake.
func TestLLMTransportNegotiatesHTTP2AndKeepsTheConnectionAfterACancel(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			if r.Context().Err() != nil {
				return
			}
			_, _ = io.WriteString(w, "data: {}\n\n")
			f.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newLLMTransport()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
	}
	if _, err := resp.Body.Read(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	for i := 0; i < 3; i++ {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d connections for one cancelled stream and three requests, want 1", n)
	}
}
