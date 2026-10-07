package websocket

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestIsWebSocketURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "ws:// URL",
			url:  "ws://example.com/live",
			want: true,
		},
		{
			name: "wss:// URL",
			url:  "wss://example.com/live",
			want: true,
		},
		{
			name: "http:// URL",
			url:  "http://example.com/live.flv",
			want: false,
		},
		{
			name: "https:// URL",
			url:  "https://example.com/live.m3u8",
			want: false,
		},
		{
			name: "invalid URL",
			url:  "://invalid",
			want: false,
		},
		{
			name: "empty string",
			url:  "",
			want: false,
		},
		{
			name: "ws with path and query",
			url:  "ws://example.com/live?token=abc",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWebSocketURL(tt.url)
			if got != tt.want {
				t.Errorf("isWebSocketURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestIsRetriableWS(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "error containing 403",
			err:  errors.New("websocket: close 403"),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "other error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "error with 403 in message",
			err:  errors.New("HTTP 403 Forbidden"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableWS(tt.err)
			if got != tt.want {
				t.Errorf("isRetriableWS(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestFrontendResponseHeader verifies the hijacked HTTP/1.1 response header is
// compatible with HTTP/2 reverse proxies. Sending "Transfer-Encoding:
// identity" made Caddy return 502 to HTTP/2 clients because HTTP/2 has no
// Transfer-Encoding concept. The body must be close-delimited (Connection:
// close, no Content-Length) so an indefinite live stream can be proxied.
func TestFrontendResponseHeader(t *testing.T) {
	h := frontendResponseHeader()
	s := string(h)

	if !strings.HasPrefix(s, "HTTP/1.1 200 OK\r\n") {
		t.Errorf("expected response to start with 200 OK, got %q", s)
	}
	if strings.Contains(s, "Transfer-Encoding:") {
		t.Errorf("response must not contain Transfer-Encoding (HTTP/2 incompatible), got: %q", s)
	}
	if !strings.Contains(s, "Content-Type: video/x-flv\r\n") {
		t.Errorf("expected Content-Type: video/x-flv, got %q", s)
	}
	if !strings.Contains(s, "Connection: close\r\n") {
		t.Errorf("expected Connection: close for close-delimited body, got %q", s)
	}
	if strings.Contains(s, "Content-Length:") {
		t.Errorf("response must not set Content-Length (indefinite stream), got %q", s)
	}
} // TestReadLoopNilConnDoesNotPanic verifies that ReadLoop returns cleanly when
// the underlying conn is nil instead of dereferencing a nil pointer. This is
// the direct regression for the panic observed when Close() races with
// ReadLoop and zeroes c.conn.
func TestReadLoopNilConnDoesNotPanic(t *testing.T) {
	c := &client{pipe: stream.NewPipe()}
	panicked := make(chan interface{}, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
				return
			}
			close(panicked)
		}()
		c.ReadLoop()
	}()
	select {
	case r, ok := <-panicked:
		if ok && r != nil {
			t.Fatalf("ReadLoop panicked on nil conn: %v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLoop did not return within timeout")
	}
}

// TestClientConcurrentCloseNoRace drives a real ws round trip and closes the
// client while ReadLoop is running. Under -race the unprotected c.conn access
// must not be flagged and ReadLoop must not panic.
func TestClientConcurrentCloseNoRace(t *testing.T) {
	up := ws.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.WriteMessage(ws.BinaryMessage, []byte("payload")); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http")
	cl := NewXP2PClient(u, nil, nil)
	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := cl.Read(buf); err != nil {
				return
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := cl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Give ReadLoop time to observe the closed connection and exit.
	time.Sleep(50 * time.Millisecond)
}

// stallServer is a ws test server whose first connection delivers a few
// messages and then goes silent without closing (the exact failure mode of
// DouYu xp2p edge nodes after token expiry). Subsequent connections stream
// "late" payloads on a ticker.
type stallServer struct {
	*httptest.Server
	conns atomic.Int32
}

func newStallServer(t *testing.T) *stallServer {
	t.Helper()
	up := ws.Upgrader{}
	s := &stallServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if s.conns.Add(1) == 1 {
			// First connection: a few messages, then silence forever.
			for i := 0; i < 3; i++ {
				if err := c.WriteMessage(ws.BinaryMessage, []byte("early")); err != nil {
					return
				}
			}
			select {} // hang: keep the connection open but send nothing
		}
		defer c.Close()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.WriteMessage(ws.BinaryMessage, []byte("late")); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// readUntil reads from the client until the accumulated output contains want
// or the timeout elapses. It returns what was read.
func readUntil(t *testing.T, c *client, want string, timeout time.Duration) (string, error) {
	t.Helper()
	deadline := time.After(timeout)
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		type res struct {
			n   int
			err error
		}
		ch := make(chan res, 1)
		go func() {
			n, err := c.Read(buf)
			ch <- res{n, err}
		}()
		select {
		case <-deadline:
			return sb.String(), errors.New("timeout waiting for " + want)
		case r := <-ch:
			if r.err != nil {
				return sb.String(), r.err
			}
			sb.Write(buf[:r.n])
			if strings.Contains(sb.String(), want) {
				return sb.String(), nil
			}
		}
	}
}

// readBytes reads from the client until n bytes accumulate or the timeout
// elapses. It returns what was read.
func readBytes(t *testing.T, c *client, n int, timeout time.Duration) ([]byte, error) {
	t.Helper()
	deadline := time.After(timeout)
	var out []byte
	for len(out) < n {
		type res struct {
			data []byte
			err  error
		}
		ch := make(chan res, 1)
		go func() {
			buf := make([]byte, 4096)
			rn, err := c.Read(buf)
			ch <- res{buf[:rn], err}
		}()
		select {
		case <-deadline:
			return out, errors.New("timeout reading bytes")
		case r := <-ch:
			if r.err != nil {
				return out, r.err
			}
			out = append(out, r.data...)
		}
	}
	return out, nil
}

// TestReadLoopWatchdogReconnectsOnStall is the regression for the DouYu
// xp2p stall: the edge node stops pushing data but keeps the connection
// open. Without a read deadline, ReadMessage blocks forever and the client
// sees an idle stream. The watchdog must trip and reconnect transparently.
func TestReadLoopWatchdogReconnectsOnStall(t *testing.T) {
	srv := newStallServer(t)
	url := wsURL(srv.Server)

	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: url}, nil
	}, nil, nil, "").(*client)
	cl.watchdog = 150 * time.Millisecond
	cl.backoffs = []time.Duration{5 * time.Millisecond}
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	out, err := readUntil(t, cl, "late", 3*time.Second)
	if err != nil {
		t.Fatalf("client did not recover after stall: %v (got %q)", err, out)
	}
	if got := srv.conns.Load(); got < 2 {
		t.Errorf("server saw %d connections, want >= 2 (watchdog should reconnect)", got)
	}
	if !strings.Contains(out, "early") {
		t.Errorf("expected initial payload before the stall, got %q", out)
	}
}

// TestReadLoopRefreshesBeforeExpiry verifies a URL with ExpireAt set is
// re-extracted and reconnected before it expires, even while data is still
// flowing (so the watchdog alone would not fire).
func TestReadLoopRefreshesBeforeExpiry(t *testing.T) {
	up := ws.Upgrader{}
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		defer c.Close()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			payload := strings.Repeat("x", 4096) // keep data flowing; watchdog stays fed
			if err := c.WriteMessage(ws.BinaryMessage, []byte(payload)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	url := wsURL(srv)

	expireAt := time.Now().Add(400 * time.Millisecond)
	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		e := expireAt
		return &stream.ExtractResult{URL: url, ExpireAt: &e}, nil
	}, nil, nil, "").(*client)
	cl.watchdog = 30 * time.Second // watchdog disabled for this test
	cl.refreshLead = 200 * time.Millisecond
	cl.backoffs = []time.Duration{5 * time.Millisecond}
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The refresh point is now+200ms; the URL expires at now+400ms. A second
	// connection must appear well before expiry without the stream stalling.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if conns.Load() >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no proactive refresh: server saw %d connections in 2s", conns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Data must still flow after the reconnect.
	if _, err := readUntil(t, cl, "xxxx", time.Second); err != nil {
		t.Errorf("stream broken after proactive refresh: %v", err)
	}
}

// TestReconnectGivesUpAfterMaxFails verifies that when re-extraction keeps
// failing (e.g. the room went offline), the client stops retrying after the
// configured number of attempts and closes the pipe instead of spinning.
func TestReconnectGivesUpAfterMaxFails(t *testing.T) {
	up := ws.Upgrader{}
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := up.Upgrade(w, r, nil); err != nil {
			return
		}
		conns.Add(1)
		// Accept but never send anything: trips the watchdog.
		select {}
	}))
	defer srv.Close()
	url := wsURL(srv)

	var calls atomic.Int32
	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		if calls.Add(1) == 1 {
			return &stream.ExtractResult{URL: url}, nil
		}
		return nil, errors.New("room is closed")
	}, nil, nil, "").(*client)
	cl.watchdog = 100 * time.Millisecond
	cl.backoffs = []time.Duration{1 * time.Millisecond}
	cl.maxReconnectFails = 3
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	buf := make([]byte, 16)
	done := make(chan error, 1)
	go func() {
		_, err := cl.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pipe closed without error after giving up")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipe never closed after reconnect attempts were exhausted")
	}
	if got := calls.Load(); got != 4 { // 1 initial + 3 failed retries
		t.Errorf("extractFn called %d times, want 4", got)
	}
}

// TestReadLoopReconnectsOn403 keeps the original 403-driven reconnect path
// covered: a re-dial that hits a 403 is retried with backoff and the stream
// recovers once a good URL comes back.
func TestReadLoopReconnectsOn403(t *testing.T) {
	up := ws.Upgrader{}
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if conns.Add(1) == 1 {
			// First connection delivers data then closes with a 403 close error.
			c.WriteMessage(ws.BinaryMessage, []byte("first"))
			c.WriteControl(ws.CloseMessage,
				ws.FormatCloseMessage(403, "forbidden"),
				time.Now().Add(time.Second))
			c.Close()
			return
		}
		defer c.Close()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.WriteMessage(ws.BinaryMessage, []byte("recovered")); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	base := wsURL(srv)

	var calls atomic.Int32
	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		n := calls.Add(1)
		if n == 2 {
			return &stream.ExtractResult{URL: base + "/blocked"}, nil
		}
		return &stream.ExtractResult{URL: base}, nil
	}, nil, nil, "").(*client)
	cl.watchdog = 5 * time.Second
	cl.backoffs = []time.Duration{5 * time.Millisecond}
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	out, err := readUntil(t, cl, "recovered", 3*time.Second)
	if err != nil {
		t.Fatalf("stream did not recover after 403: %v (got %q)", err, out)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("extractFn called %d times, want >= 3 (403 must be retried)", got)
	}
}

func TestIsTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("connection reset"), want: false},
		{name: "deadline exceeded", err: os.ErrDeadlineExceeded, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTimeout(tt.err); got != tt.want {
				t.Errorf("isTimeout(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// flvMediaTag is the media (non-config) audio tag carried inside
// flvMediaChunk — the part that keeps flowing once the FLV header has
// passed. A real upstream sends the header exactly once per connection and
// continues with media-only messages.
func flvMediaTag() []byte {
	return []byte{
		0x08, 0x00, 0x00, 0x01, // tag type 8 (audio), data size 1
		0x00, 0x00, 0x00, 0x00, // timestamp
		0x00, 0x00, 0x00, // stream id
		0x00,                   // tag data
		0x00, 0x00, 0x00, 0x0f, // previous tag size
	}
}

// flvMediaChunk is a self-contained FLV header plus one media (non-config)
// audio tag. HeaderCacheWriter caches and strips the header and flushes the
// media tag through to the pipe, so the client records a pipe write.
func flvMediaChunk() []byte {
	// FLV signature (3) + version (1) + flags (1) + header size (4) = 9 bytes.
	header := []byte("FLV\x01\x00\x00\x00\x00\x09")
	prevTagSize0 := []byte{0, 0, 0, 0}
	// Audio tag: type 0x08, dataSize 1, payload 0x00 (raw PCM — not a config tag).
	tagHeader := []byte{0x08, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	tagData := []byte{0x00}
	prevTagSize := []byte{0, 0, 0, 0x0f}
	return append(append(append(append(header, prevTagSize0...), tagHeader...), tagData...), prevTagSize...)
}

// flvDribbleChunk starts an FLV header plus a video tag header that declares
// a ~4KB payload, then trickles 2-byte fragments forever without ever
// completing the tag. HeaderCacheWriter keeps buffering this (detect state
// never resolves), so nothing reaches the pipe even though ws messages keep
// arriving — the "fake-alive" stream that a message-level watchdog cannot
// catch.
func flvDribbleStart() []byte {
	header := []byte("FLV\x01\x00\x00\x00\x00\x09")
	prevTagSize0 := []byte{0, 0, 0, 0}
	// Video tag header: type 0x09, dataSize 0x000F42 (3906 bytes of payload).
	tagHeader := []byte{0x09, 0x00, 0x0f, 0x42, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	return append(append(header, prevTagSize0...), tagHeader...)
}

// TestReadLoopPipeStallWatchdog is the regression for the fake-alive stream:
// a connection whose ws messages keep arriving but whose data never reaches
// the pipe (an in-progress giant FLV tag that dribbles in forever). The
// message-level watchdog stays fed; only a pipe-write-level watchdog notices.
func TestReadLoopPipeStallWatchdog(t *testing.T) {
	up := ws.Upgrader{}
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		switch conns.Add(1) {
		case 1:
			// Healthy connection: real media data flows, then silence.
			c.WriteMessage(ws.BinaryMessage, flvMediaChunk())
			select {}
		case 2:
			// Fake-alive connection: giant tag dribbles 2 bytes every 20ms,
			// ws messages keep coming but nothing ever reaches the pipe.
			c.WriteMessage(ws.BinaryMessage, flvDribbleStart())
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if err := c.WriteMessage(ws.BinaryMessage, []byte{0x00, 0x00}); err != nil {
					return
				}
			}
		default:
			defer c.Close()
			// Healthy again.
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if err := c.WriteMessage(ws.BinaryMessage, flvMediaChunk()); err != nil {
					return
				}
			}
		}
	}))
	defer srv.Close()
	url := wsURL(srv)

	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: url}, nil
	}, nil, nil, "test:pipe-stall").(*client)
	// Message-level watchdog is generous: the dribbling messages keep it fed.
	cl.watchdog = 30 * time.Second
	cl.pipeWatchdog = 300 * time.Millisecond
	cl.backoffs = []time.Duration{5 * time.Millisecond}
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The client must cycle past the fake-alive connection and recover on
	// the third one; the pipe stall (300ms) drives the reconnects, not the
	// 30s message watchdog.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if conns.Load() >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake-alive connection not replaced: server saw %d connections in 5s", conns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Data must flow again: read enough bytes to prove the third connection
	// flushed through the pipe. The marker is the media tag header — the FLV
	// signature itself no longer reaches the pipe once headers are stripped.
	if _, err := readUntil(t, cl, "\x08\x00\x00\x01", 2*time.Second); err != nil {
		t.Errorf("stream broken after pipe-stall reconnect: %v", err)
	}
}

// TestReadLoopReconnectStripsHeader is the regression test for the ws-path
// header splice: reconnect() resets the header writer, and the fresh
// connection restarts with a complete FLV stream — header included. The new
// header must be cached and stripped, because the downstream client already
// got the header from the cache; a second FLV signature in the pipe would
// desync demuxers mid-stream.
func TestReadLoopReconnectStripsHeader(t *testing.T) {
	up := ws.Upgrader{}
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		switch conns.Add(1) {
		case 1:
			// First connection: one complete FLV chunk, then silence — the
			// pipe watchdog declares it stalled and reconnects.
			c.WriteMessage(ws.BinaryMessage, flvMediaChunk())
			select {}
		default:
			// Reconnected: like a fresh upstream connection, the stream
			// restarts with a complete FLV chunk (header + media tag), then
			// continues with media-only messages.
			if err := c.WriteMessage(ws.BinaryMessage, flvMediaChunk()); err != nil {
				return
			}
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if err := c.WriteMessage(ws.BinaryMessage, flvMediaTag()); err != nil {
					return
				}
			}
		}
	}))
	defer srv.Close()
	url := wsURL(srv)

	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: url}, nil
	}, nil, nil, "test:reconnect-strip").(*client)
	cl.watchdog = 30 * time.Second
	cl.pipeWatchdog = 300 * time.Millisecond
	cl.backoffs = []time.Duration{5 * time.Millisecond}
	defer cl.Close()

	if err := cl.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read well past the first connection's single chunk (16 media bytes) so
	// the assertion covers data delivered by the reconnected stream.
	out, err := readBytes(t, cl, 64, 3*time.Second)
	if err != nil || len(out) < 64 {
		t.Fatalf("stream broken after reconnect: %v (got %d bytes)", err, len(out))
	}
	if got := strings.Count(string(out), "FLV"); got != 0 {
		t.Errorf("pipe output contains %d FLV signatures, want 0 (reconnected stream's header must be stripped)", got)
	}
}

// TestReadDeadlinePipeStall verifies the deadline computation itself: once a
// pipe write has happened, the read deadline is capped by the pipe watchdog
// even when the message watchdog would allow much longer.
func TestReadDeadlinePipeStall(t *testing.T) {
	cl := &client{pipe: stream.NewPipe()}
	cl.watchdog = 30 * time.Second
	cl.pipeWatchdog = 5 * time.Second

	// No pipe write yet: deadline is the message watchdog.
	d := cl.readDeadline()
	if got := time.Until(d); got < 25*time.Second || got > 30*time.Second {
		t.Errorf("readDeadline() = %v from now, want ~30s (message watchdog)", got)
	}

	// After a pipe write: deadline is capped by the pipe watchdog.
	cl.markPipeWrite()
	d = cl.readDeadline()
	if got := time.Until(d); got < 4*time.Second || got > 5*time.Second {
		t.Errorf("readDeadline() = %v from now, want ~5s (pipe watchdog)", got)
	}
}

// TestReconnectStopsOnFormatDeadlock verifies that a terminal format
// deadlock (the platform permanently switched the room away from ws, e.g.
// DouYu pulling it back to http CDN) stops the reconnect loop immediately
// instead of burning the whole retry budget on attempts that cannot succeed.
func TestReconnectStopsOnFormatDeadlock(t *testing.T) {
	var calls atomic.Int32
	cl := NewXP2PClientWithRetry(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		calls.Add(1)
		return nil, fmt.Errorf("format changed from ws to flv: %w", stream.ErrFormatDeadlock)
	}, nil, nil, "").(*client)
	// A generous budget: if the deadlock sentinel is ignored, the loop runs
	// all 10 attempts and the call count exposes the bug.
	cl.maxReconnectFails = 10
	cl.backoffs = []time.Duration{1 * time.Millisecond}
	defer cl.Close()

	if cl.reconnect() {
		t.Fatal("reconnect must fail on format deadlock")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("extractFn called %d times, want 1 (deadlock must stop retries immediately)", got)
	}
	err := cl.pipe.Err()
	if err == nil {
		t.Fatal("pipe must be closed with the deadlock error")
	}
	if !errors.Is(err, stream.ErrFormatDeadlock) {
		t.Errorf("expected pipe error wrapping ErrFormatDeadlock, got: %v", err)
	}
}
