package stream

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestPipe_ReadWrite(t *testing.T) {
	p := NewPipe()

	// Write data in a goroutine since Read blocks.
	go func() {
		p.Write([]byte("hello"))
		p.CloseWithError(io.EOF)
	}()

	buf := make([]byte, 10)
	n, err := p.Read(buf)
	if err != nil {
		t.Fatalf("Read returned unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("Read got %q, want %q", string(buf[:n]), "hello")
	}

	// After CloseWithError, next Read should return the error.
	_, err = p.Read(buf)
	if err != io.EOF {
		t.Fatalf("Read after CloseWithError got %v, want io.EOF", err)
	}
}

func TestPipe_BreakWithError(t *testing.T) {
	p := NewPipe()

	// Write some data first.
	_, err := p.Write([]byte("data"))
	if err != nil {
		t.Fatalf("Write returned unexpected error: %v", err)
	}

	// BreakWithError should cause immediate read error, even with unread data.
	breakErr := errors.New("break")
	p.BreakWithError(breakErr)

	buf := make([]byte, 10)
	_, err = p.Read(buf)
	if err != breakErr {
		t.Fatalf("Read after BreakWithError got %v, want %v", err, breakErr)
	}

	// Subsequent Write should fail.
	_, err = p.Write([]byte("more"))
	if err == nil {
		t.Fatal("Write after BreakWithError should fail")
	}
}

func TestPipe_CloseWithError(t *testing.T) {
	p := NewPipe()

	// Write data first.
	_, err := p.Write([]byte("buffered"))
	if err != nil {
		t.Fatalf("Write returned unexpected error: %v", err)
	}

	// CloseWithError should let the reader drain the buffer first,
	// then return the error.
	closeErr := errors.New("closed")
	p.CloseWithError(closeErr)

	// First read should get the buffered data.
	buf := make([]byte, 100)
	n, err := p.Read(buf)
	if err != nil {
		t.Fatalf("Read got unexpected error: %v", err)
	}
	if string(buf[:n]) != "buffered" {
		t.Fatalf("Read got %q, want %q", string(buf[:n]), "buffered")
	}

	// Next read should return the close error.
	_, err = p.Read(buf)
	if err != closeErr {
		t.Fatalf("Read after drain got %v, want %v", err, closeErr)
	}
}

func TestFormatMatches(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "same scheme and extension",
			a:    "https://example.com/live/stream.flv",
			b:    "https://example.com/live/stream2.flv",
			want: true,
		},
		{
			name: "different scheme",
			a:    "https://example.com/live/stream.flv",
			b:    "http://example.com/live/stream.flv",
			want: false,
		},
		{
			name: "different extension",
			a:    "https://example.com/live/stream.flv",
			b:    "https://example.com/live/stream.m3u8",
			want: false,
		},
		{
			name: "invalid URL a",
			a:    "://invalid",
			b:    "https://example.com/live/stream.flv",
			want: false,
		},
		{
			name: "invalid URL b",
			a:    "https://example.com/live/stream.flv",
			b:    "://invalid",
			want: false,
		},
		{
			name: "ws scheme match",
			a:    "ws://example.com/live",
			b:    "ws://example.com/live2",
			want: true,
		},
		{
			name: "no extension both sides",
			a:    "ws://example.com/live",
			b:    "ws://example.com/live2",
			want: true,
		},
		{
			name: "flv to xs is the same http-flv family",
			a:    "https://cdn.example.com/live/stream.flv?sign=1",
			b:    "https://hlsh5p2.douyucdn2.cn/live/stream.xs?sign=2",
			want: true,
		},
		{
			name: "flv to m3u8 stays different",
			a:    "https://cdn.example.com/live/stream.flv",
			b:    "https://cdn.example.com/live/stream.m3u8",
			want: false,
		},
		{
			name: "one has extension other does not",
			a:    "ws://example.com/live",
			b:    "ws://example.com/live.flv",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatMatches(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("formatMatches(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestIsRetriable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "error containing 403",
			err:  errors.New("HTTP 403 Forbidden"),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "connection refused is transient",
			err:  errors.New("connection refused"),
			want: true,
		},
		{
			name: "error with 403 in middle",
			err:  errors.New("got status 403"),
			want: true,
		},
		{
			name: "404 means URL is stale, re-extract heals",
			err:  errors.New("err got: 404 Not Found"),
			want: true,
		},
		{
			name: "410 gone means URL is stale",
			err:  errors.New("err got: 410 Gone"),
			want: true,
		},
		{
			name: "500 server error is transient",
			err:  errors.New("err got: 500 Internal Server Error"),
			want: true,
		},
		{
			name: "502 bad gateway is transient",
			err:  errors.New("err got: 502 Bad Gateway"),
			want: true,
		},
		{
			name: "503 service unavailable is transient",
			err:  errors.New("err got: 503 Service Unavailable"),
			want: true,
		},
		{
			name: "connection reset by peer is transient",
			err:  errors.New("read: connection reset by peer"),
			want: true,
		},
		{
			name: "unexpected EOF is transient",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "timeout is transient",
			err:  errors.New("net/http: TLS handshake timeout"),
			want: true,
		},
		{
			name: "server closed idle connection is transient",
			err:  errors.New("http: server closed idle connection"),
			want: true,
		},
		{
			name: "deterministic error stays fatal",
			err:  errors.New("parse extracted URL error: invalid"),
			want: false,
		},
		{
			name: "400 stays fatal",
			err:  errors.New("err got: 400 Bad Request"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriable(tt.err)
			if got != tt.want {
				t.Errorf("isRetriable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// errReadCloser returns err from every Read, simulating an upstream body
// that dies mid-copy (e.g. connection reset).
type errReadCloser struct{ err error }

func (e *errReadCloser) Read(p []byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error               { return nil }

// blockingReadCloser blocks in Read until Close is called, simulating a
// long-lived upstream body whose data flows indefinitely.
type blockingReadCloser struct{ closed chan struct{} }

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	<-b.closed
	return 0, errors.New("read on closed body")
}

func (b *blockingReadCloser) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func setTestBackoff(t *testing.T, base time.Duration) {
	t.Helper()
	old := retryBackoffBase
	retryBackoffBase = base
	t.Cleanup(func() { retryBackoffBase = old })
}

// TestStreamRetriableReconnect verifies that an upstream error mid-copy
// that is retriable (connection reset) causes a re-extract and reconnect
// instead of closing the client stream.
func TestStreamRetriableReconnect(t *testing.T) {
	setTestBackoff(t, 10*time.Millisecond)

	var extractCount int32
	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		atomic.AddInt32(&extractCount, 1)
		return &ExtractResult{URL: "http://upstream.example.com/live/stream.flv"}, nil
	}

	var fetchCount int32
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		switch atomic.AddInt32(&fetchCount, 1) {
		case 1:
			// First upstream connection dies mid-copy with a reset.
			return &errReadCloser{err: errors.New("read tcp: connection reset by peer")}, nil
		default:
			// Second connection delivers data then EOFs (clean reconnect).
			return io.NopCloser(strings.NewReader("chunk")), nil
		}
	}

	s := NewStream(extractFn, fetchFn)
	defer s.Close()

	buf := make([]byte, 16)
	deadline := time.After(3 * time.Second)
	for {
		n, err := s.Read(buf)
		if err != nil {
			t.Fatalf("Read returned error: %v", err)
		}
		if string(buf[:n]) == "chunk" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reconnect data")
		default:
		}
	}

	if got := atomic.LoadInt32(&extractCount); got < 2 {
		t.Errorf("expected re-extract after reset, extract called %d times", got)
	}
	if got := atomic.LoadInt32(&fetchCount); got < 2 {
		t.Errorf("expected refetch after reset, fetch called %d times", got)
	}
}

// TestStreamExtractBackoff verifies that repeated extract failures back
// off instead of hot-looping against the platform API.
func TestStreamExtractBackoff(t *testing.T) {
	setTestBackoff(t, 50*time.Millisecond)

	var extractCount int32
	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		atomic.AddInt32(&extractCount, 1)
		return nil, errors.New("play info api error code -412: request rejected")
	}
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		t.Fatal("fetch must not be called when extract fails")
		return nil, nil
	}

	s := NewStream(extractFn, fetchFn)
	defer s.Close()

	// With a 50ms base backoff (0, 50, 150, 350 ...), ~500ms should see
	// only a handful of calls. A hot loop would hit thousands.
	time.Sleep(500 * time.Millisecond)
	got := atomic.LoadInt32(&extractCount)
	if got > 6 {
		t.Errorf("extract called %d times in 500ms, backoff not applied (hot loop)", got)
	}
	if got < 2 {
		t.Errorf("extract called only %d times in 500ms, expected retries", got)
	}
}

// TestStreamBackoffInterruptedByClose verifies Close unblocks a produce
// goroutine sleeping between retries, so no goroutine lingers after the
// client disconnects.
func TestStreamBackoffInterruptedByClose(t *testing.T) {
	setTestBackoff(t, 30*time.Second)

	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		return nil, errors.New("extract error")
	}
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		return nil, nil
	}

	s := NewStream(extractFn, fetchFn)

	// Let the first extract fail so produce enters the backoff sleep.
	time.Sleep(50 * time.Millisecond)
	s.Close()

	waitCh := make(chan error, 1)
	go func() { waitCh <- s.Wait() }()
	select {
	case <-waitCh:
		// Produce goroutine exited promptly despite the long backoff.
	case <-time.After(2 * time.Second):
		t.Fatal("Wait timed out: backoff sleep not interrupted by Close")
	}
}

// TestStreamProactiveRefresh verifies that when the extract result carries
// an ExpireAt, the produce loop closes the upstream body shortly before
// expiry and re-extracts for a fresh URL instead of waiting for a 403.
func TestStreamProactiveRefresh(t *testing.T) {
	setTestBackoff(t, 10*time.Millisecond)

	var extractCount int32
	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		if atomic.AddInt32(&extractCount, 1) == 1 {
			exp := time.Now().Add(300 * time.Millisecond)
			return &ExtractResult{
				URL:      "http://upstream.example.com/live/stream.flv",
				ExpireAt: &exp,
			}, nil
		}
		return &ExtractResult{URL: "http://upstream.example.com/live/stream.flv"}, nil
	}

	var fetchCount int32
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		if atomic.AddInt32(&fetchCount, 1) == 1 {
			return newBlockingReadCloser(), nil
		}
		return io.NopCloser(strings.NewReader("fresh")), nil
	}

	s := NewStream(extractFn, fetchFn)
	defer s.Close()

	buf := make([]byte, 16)
	deadline := time.After(3 * time.Second)
	for {
		n, err := s.Read(buf)
		if err != nil {
			t.Fatalf("Read returned error: %v", err)
		}
		if string(buf[:n]) == "fresh" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for proactive refresh data")
		default:
		}
	}

	if got := atomic.LoadInt32(&extractCount); got < 2 {
		t.Errorf("expected proactive re-extract before expiry, extract called %d times", got)
	}
}

// TestStreamNoExpiryNoRefresh verifies that results without ExpireAt never
// trigger a proactive close: a healthy long-lived body keeps flowing.
func TestStreamNoExpiryNoRefresh(t *testing.T) {
	setTestBackoff(t, 10*time.Millisecond)

	body := newBlockingReadCloser()
	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		return &ExtractResult{URL: "http://upstream.example.com/live/stream.flv"}, nil
	}
	var fetchCount int32
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		atomic.AddInt32(&fetchCount, 1)
		return body, nil
	}

	s := NewStream(extractFn, fetchFn)
	defer s.Close()

	// Give the produce loop plenty of time to (wrongly) refresh if buggy.
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Errorf("fetch called %d times without ExpireAt, want 1 (no proactive refresh)", got)
	}
}

func TestStream_Close(t *testing.T) {
	// Create a Stream with extract and fetch functions that block,
	// then verify Close stops the produce goroutine.
	extractCalled := make(chan struct{})
	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		extractCalled <- struct{}{}
		// Block to keep produce loop waiting.
		select {}
	}

	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		// Should not be reached since extract blocks.
		return nil, errors.New("unexpected fetch call")
	}

	s := NewStream(extractFn, fetchFn)

	// Wait for the produce goroutine to call extractFn.
	select {
	case <-extractCalled:
		// Good, produce goroutine is running.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for extractFn to be called")
	}

	// Close the stream — this should break the pipe and stop produce.
	err := s.Close()
	if err != nil {
		t.Fatalf("Close returned unexpected error: %v", err)
	}

	// Wait should return promptly.
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- s.Wait()
	}()

	select {
	case <-waitCh:
		// Produce goroutine stopped.
	case <-time.After(2 * time.Second):
		t.Fatal("Wait timed out, produce goroutine may not have stopped")
	}
}

// TestStreamFormatDeadlockCloses verifies that when the extractFn reports a
// permanent format change (ErrFormatDeadlock, e.g. DouYu switching a room to
// p2p mid-stream), the produce loop stops retrying and closes the client
// stream instead of looping forever.
func TestStreamFormatDeadlockCloses(t *testing.T) {
	setTestBackoff(t, 10*time.Millisecond)

	extractFn := func(previous *ExtractResult) (*ExtractResult, error) {
		return nil, fmt.Errorf("format changed from flv to ws: %w", ErrFormatDeadlock)
	}
	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		return nil, nil
	}

	s := NewStream(extractFn, fetchFn)

	waitCh := make(chan error, 1)
	go func() { waitCh <- s.Wait() }()
	select {
	case err := <-waitCh:
		if err == nil {
			t.Error("expected Wait to return the deadlock error, got nil")
		}
		if !errors.Is(err, ErrFormatDeadlock) {
			t.Errorf("expected error wrapping ErrFormatDeadlock, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream never closed: format deadlock loops forever")
	}

	// The client must see the error on Read (stream closed, not parked).
	buf := make([]byte, 8)
	if _, err := s.Read(buf); err == nil {
		t.Error("expected Read to return the close error after deadlock")
	}
}
