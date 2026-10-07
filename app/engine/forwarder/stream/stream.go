package stream

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// ExtractFunc is called to obtain a fresh stream URL from a platform extractor.
// previous is nil on the first call, and set to the previous result on retries,
// so the extractor can ensure format consistency.
type ExtractFunc func(previous *ExtractResult) (*ExtractResult, error)

// ErrFormatDeadlock marks an extraction error that retrying provably cannot
// resolve: the platform has permanently switched this room to a different
// protocol mid-stream (e.g. DouYu moving a room to p2p/ws while an FLV
// session is already underway). The HTTP response is already committed to
// the original format, so the only recovery is for the client to reconnect,
// which re-dispatches into the forwarder matching the new format. Producers
// receiving an error wrapping this sentinel must close the stream instead of
// looping forever.
var ErrFormatDeadlock = errors.New("stream format changed permanently")

// ExtractResult holds the resolved URL and optional headers needed to fetch it.
type ExtractResult struct {
	URL             string
	Headers         http.Header
	ExpireAt        *time.Time // when the URL expires; nil means unknown or no expiry
	VariantSelector any        // optional func([]*libm3u8.Variant) *libm3u8.Variant; used by HLS forwarder
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

// Retry backoff bounds: every failure in the produce loop sleeps at least
// retryBackoffBase (doubling up to retryBackoffMax) before the next attempt,
// so repeated upstream/API failures can never hot-loop against the platform
// API (which would trigger rate-limiting and turn a transient hiccup into an
// outage). Package-level var so tests can shorten it.
var (
	retryBackoffBase = 1 * time.Second
	retryBackoffMax  = 10 * time.Second
)

// refreshLeadTime is how long before ExtractResult.ExpireAt the producer
// proactively drops the upstream connection and re-extracts a fresh URL,
// instead of waiting for the CDN to start rejecting the stale one.
const refreshLeadTime = 60 * time.Second

func (s *Stream) produce(extractFn ExtractFunc, fetchFn FetchFunc) {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.produce")
	var previous *ExtractResult
	backoff := retryBackoffBase

	for {
		result, err := extractFn(previous)
		if err != nil {
			if errors.Is(err, ErrFormatDeadlock) {
				// The platform switched this room to another protocol for
				// good; no retry can fix it. Close so the client reconnects
				// into the forwarder matching the new format.
				log.Errorf("extract format deadlock, closing stream: %s", err.Error())
				s.closeWithError(err)
				return
			}
			log.Warnf("extract error: %s", err.Error())
			if !s.sleepBackoff(&backoff) {
				return
			}
			continue
		}

		// On retry: validate that the new URL format matches the initial one.
		if previous != nil && !formatMatches(previous.URL, result.URL) {
			log.Warnf("extract returned different format (was %s, got %s), retrying", previous.URL, result.URL)
			if !s.sleepBackoff(&backoff) {
				return
			}
			continue
		}

		body, err := fetchFn(result.URL, result.Headers)
		if err != nil {
			if isRetriable(err) {
				log.Warnf("fetch retriable error: %s", err.Error())
				if !s.sleepBackoff(&backoff) {
					return
				}
				continue
			}
			s.closeWithError(err)
			return
		}

		previous = result
		backoff = retryBackoffBase // extract + fetch succeeded, reset backoff

		var w io.Writer = s.pipe
		if s.writerWrapper != nil {
			w = s.writerWrapper(s.pipe)
		}

		// Proactive refresh: when the URL carries an expiry, drop the
		// connection shortly before it goes stale and re-extract, so the
		// handover happens while the CDN still serves the old token.
		refreshed := &atomic.Bool{}
		var refreshTimer *time.Timer
		if result.ExpireAt != nil {
			remaining := time.Until(*result.ExpireAt)
			lead := refreshLeadTime
			// Short-lived tokens: refresh 5s before expiry at the latest.
			if remaining < lead+5*time.Second {
				lead = remaining - 5*time.Second
				if lead < 0 {
					lead = 0
				}
			}
			when := remaining - lead
			log.Debugf("url expires at %s, scheduling proactive refresh in %s", result.ExpireAt.Format(time.RFC3339), when)
			refreshTimer = time.AfterFunc(when, func() {
				refreshed.Store(true)
				body.Close() // unblocks a Read parked inside io.Copy
			})
		}

		_, err = io.Copy(w, body)
		if refreshTimer != nil {
			refreshTimer.Stop()
		}
		body.Close()

		if s.pipe.Err() != nil {
			// Pipe was closed from the consumer side (client disconnected).
			return
		}

		if refreshed.Load() {
			// Timer-initiated close: reconnect with a fresh URL, not a failure.
			log.Infoln("proactive refresh triggered, re-extracting")
			continue
		}

		if err != nil {
			if isRetriable(err) {
				log.Warnf("copy retriable error: %s", err.Error())
				if !s.sleepBackoff(&backoff) {
					return
				}
				continue
			}
			s.closeWithError(err)
			return
		}

		// io.Copy returned nil — upstream closed cleanly. Re-extract and reconnect.
		log.Debugln("upstream closed cleanly, re-extracting")
	}
}

// sleepBackoff waits for the current backoff interval, doubling it (up to
// retryBackoffMax) for the next failure. Returns false when the stream was
// closed while sleeping, meaning the caller must stop producing.
func (s *Stream) sleepBackoff(backoff *time.Duration) bool {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.sleepBackoff")
	log.Debugf("retrying in %s", *backoff)
	t := time.NewTimer(*backoff)
	defer t.Stop()
	*backoff *= 2
	if *backoff > retryBackoffMax {
		*backoff = retryBackoffMax
	}
	select {
	case <-t.C:
		return true
	case <-s.done:
		return false
	}
}

func (s *Stream) closeWithError(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.stream.closeWithError")
	log.Warnf("closing stream with error: %s", err.Error())
	s.closeErr = err
	s.pipe.CloseWithError(err)
	close(s.done)
}

// formatMatches checks that two URLs have the same scheme and path extension
// family, so re-extraction doesn't switch between FLV/HLS/WebSocket
// mid-stream. DouYu p2p=2 serves the same http-flv payload under a ".xs"
// extension, so .xs and .flv count as the same format.
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
	if normalizeExt(path.Ext(ua.Path)) != normalizeExt(path.Ext(ub.Path)) {
		log := global.Log.WithField("func", "app.engine.forwarder.stream.formatMatches")
		log.Debugf("extension mismatch: %s vs %s", path.Ext(ua.Path), path.Ext(ub.Path))
		return false
	}
	return true
}

// normalizeExt folds extensions serving identical payloads into one family.
func normalizeExt(ext string) string {
	if ext == ".xs" {
		return ".flv"
	}
	return ext
}

// retriablePatterns are error fragments that mark an upstream failure worth
// retrying with a fresh extraction: stale-URL status codes the CDN answers
// once a token dies (403/404/410), server-side faults (5xx), and transient
// network conditions. Everything else (e.g. a 400) is treated as fatal.
var retriablePatterns = []string{
	"403", "404", "410",
	"500", "502", "503", "504",
	"connection reset",
	"connection refused",
	"connection aborted",
	"unexpected eof",
	"broken pipe",
	"server closed",
	"context deadline",
	"timeout",
	"temporary failure",
}

// isRetriable checks if an error indicates a condition that a fresh
// extraction can heal: URL expiry (403/404/410), upstream server faults
// (5xx) or transient network failures (reset/EOF/timeout). Only errors
// that are deterministic — where retrying provably cannot help — stay
// fatal and close the client stream.
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, pattern := range retriablePatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// Ensure ExtractResult is usable — the fmt import is needed for potential
// future error formatting in this package.
var _ = fmt.Sprintf
