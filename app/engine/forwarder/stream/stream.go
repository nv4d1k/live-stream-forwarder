package stream

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// ExtractFunc is called to obtain a fresh stream URL from a platform extractor.
// previous is nil on the first call, and set to the previous result on retries,
// so the extractor can ensure format consistency.
type ExtractFunc func(previous *ExtractResult) (*ExtractResult, error)

// ExtractResult holds the resolved URL and optional headers needed to fetch it.
type ExtractResult struct {
	URL     string
	Headers http.Header
}

// FetchFunc returns the upstream response body for a given URL.
type FetchFunc func(u string, headers http.Header) (io.ReadCloser, error)

// WriterWrapperFunc wraps an io.Writer with additional behavior.
// Called once per produce iteration to create the writer target for io.Copy.
type WriterWrapperFunc func(io.Writer) io.Writer

// StreamOption configures a Stream during creation.
type StreamOption func(*Stream)

// WithWriterWrapper sets a function that wraps the pipe writer on each
// produce iteration, allowing interception of data written to the pipe
// (e.g. for FLV header caching).
func WithWriterWrapper(fn WriterWrapperFunc) StreamOption {
	return func(s *Stream) { s.writerWrapper = fn }
}

// Stream wraps a Pipe so that a consumer reads continuously while a producer
// goroutine feeds data in. When the producer encounters a 403 (URL expired),
// it calls the ExtractFunc to get a fresh URL and reconnects — the consumer
// never sees a break.
type Stream struct {
	pipe          *Pipe
	done          chan struct{}
	closeErr      error
	closeOnce     sync.Once
	writerWrapper WriterWrapperFunc
}

// NewStream creates a Stream and starts the producer goroutine.
// extractFn is called on first connect and on every 403 retry.
// fetchFn is called to actually fetch the stream data given a URL.
// opts can be used to configure the stream (e.g. WithWriterWrapper).
func NewStream(extractFn ExtractFunc, fetchFn FetchFunc, opts ...StreamOption) *Stream {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.NewStream")
	log.Debugln("creating stream")
	s := &Stream{
		pipe: NewPipe(),
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	go s.produce(extractFn, fetchFn)
	return s
}

// Read implements io.Reader. Blocks until data is available or the stream ends.
func (s *Stream) Read(p []byte) (int, error) {
	return s.pipe.Read(p)
}

// Close terminates the stream.
func (s *Stream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.Close")
	log.Debugln("closing stream")
	s.closeOnce.Do(func() {
		s.pipe.BreakWithError(io.ErrClosedPipe)
		close(s.done)
	})
	return nil
}

// Wait blocks until the producer goroutine finishes and returns the final error.
func (s *Stream) Wait() error {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.Wait")
	log.Debugln("waiting for stream to finish")
	<-s.done
	return s.closeErr
}

func (s *Stream) produce(extractFn ExtractFunc, fetchFn FetchFunc) {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.produce")
	var previous *ExtractResult

	for {
		result, err := extractFn(previous)
		if err != nil {
			log.Warnf("extract error: %s", err.Error())
			continue
		}

		// On retry: validate that the new URL format matches the initial one.
		if previous != nil && !formatMatches(previous.URL, result.URL) {
			log.Warnf("extract returned different format (was %s, got %s), retrying", previous.URL, result.URL)
			continue
		}

		body, err := fetchFn(result.URL, result.Headers)
		if err != nil {
			if isRetriable(err) {
				log.Warnf("fetch retriable error: %s", err.Error())
				continue
			}
			s.closeWithError(err)
			return
		}

		previous = result
		var w io.Writer = s.pipe
		if s.writerWrapper != nil {
			w = s.writerWrapper(s.pipe)
		}
		_, err = io.Copy(w, body)
		body.Close()

		if s.pipe.Err() != nil {
			// Pipe was closed from the consumer side (client disconnected).
			return
		}

		if err != nil {
			if isRetriable(err) {
				log.Warnf("copy retriable error: %s", err.Error())
				continue
			}
			s.closeWithError(err)
			return
		}

		// io.Copy returned nil — upstream closed cleanly. Re-extract and reconnect.
		log.Debugln("upstream closed cleanly, re-extracting")
	}
}

func (s *Stream) closeWithError(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.closeWithError")
	log.Warnf("closing stream with error: %s", err.Error())
	s.closeErr = err
	s.pipe.CloseWithError(err)
	close(s.done)
}

// formatMatches checks that two URLs have the same scheme and path extension,
// so re-extraction doesn't switch between FLV/HLS/WebSocket mid-stream.
func formatMatches(a, b string) bool {
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil {
		return false
	}
	if ua.Scheme != ub.Scheme {
		log := global.Log.WithField("func", "app.engine.forwarder.stream.formatMatches")
		log.Debugf("scheme mismatch: %s vs %s", ua.Scheme, ub.Scheme)
		return false
	}
	if path.Ext(ua.Path) != path.Ext(ub.Path) {
		log := global.Log.WithField("func", "app.engine.forwarder.stream.formatMatches")
		log.Debugf("extension mismatch: %s vs %s", path.Ext(ua.Path), path.Ext(ub.Path))
		return false
	}
	return true
}

// isRetriable checks if an error indicates a 403 (URL expired) or other
// retriable condition.
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "403")
}

// Ensure ExtractResult is usable — the fmt import is needed for potential
// future error formatting in this package.
var _ = fmt.Sprintf
