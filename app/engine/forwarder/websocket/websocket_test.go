package websocket

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
