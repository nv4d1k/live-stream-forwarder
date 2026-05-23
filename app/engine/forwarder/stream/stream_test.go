package stream

import (
	"errors"
	"io"
	"net/http"
	"os"
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
			name: "other error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "error with 403 in middle",
			err:  errors.New("got status 403"),
			want: true,
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
