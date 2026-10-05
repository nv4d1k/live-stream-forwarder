package websocket

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ws "github.com/gorilla/websocket"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/flv"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// errClientClosed is returned by DialContext when Close has already been
// invoked, so ReadLoop's reconnect path can tell an intentional shutdown apart
// from an upstream failure.
var errClientClosed = errors.New("websocket client closed")

const (
	// defaultWatchdog is how long a ws connection may stay silent before the
	// client treats it as dead. Some edge nodes (e.g. DouYu xp2p) stop pushing
	// data after token expiry but keep the connection open, so without a read
	// deadline ReadMessage blocks forever and the client sees an idle stream.
	defaultWatchdog = 30 * time.Second
	// defaultPipeWatchdog is how long the pipe may go without receiving data
	// before the connection is considered dead — measured from the last write
	// that actually reached the pipe, not from the last ws message. A stream
	// whose messages keep arriving but never reach the pipe (e.g. an
	// incomplete FLV tag being buffered forever by the header cache writer)
	// is "fake alive" and only this watchdog catches it. It is longer than
	// the message watchdog because header detection may legitimately hold
	// data back briefly.
	defaultPipeWatchdog = 60 * time.Second
	// defaultRefreshLead is how long before ExpireAt a URL is replaced with a
	// fresh one, so the handover happens while data is still flowing.
	defaultRefreshLead = 60 * time.Second
	// minReadDeadline keeps a stale ExpireAt from arming a deadline in the
	// past and spinning the read loop on immediate timeouts.
	minReadDeadline = 5 * time.Second
	// dialTimeout bounds each (re)connect handshake.
	dialTimeout = 15 * time.Second
)

// defaultReconnectBackoffs is the sleep schedule between failed reconnect
// attempts (extract or dial errors), capped at the last value.
var defaultReconnectBackoffs = []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second}

// defaultMaxReconnectFails is the number of consecutive failed reconnect
// attempts before the stream is given up and the pipe closed. The schedule
// above gives the upstream roughly half a minute to recover.
const defaultMaxReconnectFails = 5

func NewXP2PClient(u string, header http.Header, proxy *url.URL) Background {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewXP2PClient")
	log.WithField("url", u).Debug("creating XP2PClient")
	c := &client{
		url:    u,
		header: header,
		dialer: &ws.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS10,
				MaxVersion:         tls.VersionTLS13,
				CurvePreferences: []tls.CurveID{
					tls.CurveP256,
					tls.X25519,
					tls.CurveP384,
					tls.CurveP521,
				},
				CipherSuites: []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
					tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
					tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				},
			},
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
		},
		pipe: stream.NewPipe(),
	}
	if proxy != nil {
		c.dialer.Proxy = http.ProxyURL(proxy)
	}
	runtime.SetFinalizer(c, func(c *client) {
		c.Close()
	})
	return c
}

// NewXP2PClientWithRetry creates a client that will reconnect with a new URL
// from extractFn when the connection fails with a retriable error (e.g. 403).
// cacheKey enables FLV header caching; empty string disables it.
func NewXP2PClientWithRetry(extractFn stream.ExtractFunc, header http.Header, proxy *url.URL, cacheKey string) Background {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewXP2PClientWithRetry")
	log.WithField("cacheKey", cacheKey).Debug("creating XP2PClientWithRetry")
	c := &client{
		header: header,
		dialer: &ws.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS10,
				MaxVersion:         tls.VersionTLS13,
				CurvePreferences: []tls.CurveID{
					tls.CurveP256,
					tls.X25519,
					tls.CurveP384,
					tls.CurveP521,
				},
				CipherSuites: []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
					tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
					tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				},
			},
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
		},
		pipe:      stream.NewPipe(),
		extractFn: extractFn,
		cacheKey:  cacheKey,
	}
	if proxy != nil {
		c.dialer.Proxy = http.ProxyURL(proxy)
	}
	runtime.SetFinalizer(c, func(c *client) {
		c.Close()
	})
	return c
}

type client struct {
	mu           sync.Mutex
	url          string
	header       http.Header
	dialer       *ws.Dialer
	conn         *ws.Conn
	closed       bool // set by Close; ReadLoop checks it to avoid reconnecting after a client disconnect
	pipe         *stream.Pipe
	extractFn    stream.ExtractFunc
	previous     *stream.ExtractResult
	cacheKey     string
	headerWriter *flv.HeaderCacheWriter

	// pipeWriteNano is the UnixNano timestamp of the last write that actually
	// reached the pipe (0 = never). It feeds the pipe-stall watchdog, which
	// is what detects "fake alive" streams whose ws messages keep arriving
	// but never produce consumable data.
	pipeWriteNano atomic.Int64

	// Reconnect tuning. Zero values fall back to the package defaults; tests
	// override these to keep the suite fast.
	watchdog          time.Duration // max ws-message silence before a connection is considered dead
	pipeWatchdog      time.Duration // max pipe-write silence before a connection is considered dead
	refreshLead       time.Duration // how long before ExpireAt to swap in a fresh URL
	backoffs          []time.Duration
	maxReconnectFails int
}

// watchdogDuration returns the effective no-data timeout.
func (c *client) watchdogDuration() time.Duration {
	if c.watchdog > 0 {
		return c.watchdog
	}
	return defaultWatchdog
}

// pipeWatchdogDuration returns the effective pipe-stall timeout.
func (c *client) pipeWatchdogDuration() time.Duration {
	if c.pipeWatchdog > 0 {
		return c.pipeWatchdog
	}
	return defaultPipeWatchdog
}

// markPipeWrite records that data just reached the pipe.
func (c *client) markPipeWrite() {
	c.pipeWriteNano.Store(time.Now().UnixNano())
}

// lastPipeWrite returns the time of the last write that reached the pipe,
// or the zero time if there has been none.
func (c *client) lastPipeWrite() time.Time {
	if n := c.pipeWriteNano.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// refreshLeadDuration returns the effective proactive-refresh lead time.
func (c *client) refreshLeadDuration() time.Duration {
	if c.refreshLead > 0 {
		return c.refreshLead
	}
	return defaultRefreshLead
}

// backoffAt returns the sleep before the given reconnect attempt (1-based).
func (c *client) backoffAt(attempt int) time.Duration {
	b := c.backoffs
	if len(b) == 0 {
		b = defaultReconnectBackoffs
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(b) {
		attempt = len(b)
	}
	return b[attempt-1]
}

// maxFails returns the effective consecutive-failure limit.
func (c *client) maxFails() int {
	if c.maxReconnectFails > 0 {
		return c.maxReconnectFails
	}
	return defaultMaxReconnectFails
}

func (c *client) Start() error {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.client.Start")
	if c.extractFn != nil {
		result, err := c.extractFn(c.previous)
		if err != nil {
			return fmt.Errorf("extract for websocket error: %w", err)
		}
		c.url = result.URL
		c.previous = result
		log.WithField("field", "extracted url").Debug(c.url)
	}

	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	err := c.DialContext(ctx)
	if err != nil {
		return fmt.Errorf("dial context error: %w", err)
	}
	go c.ReadLoop()
	return nil
}

func (c *client) DialContext(ctx context.Context) error {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.client.DialContext")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClientClosed
	}

	conn, resp, err := c.dialer.DialContext(ctx, c.url, c.header)
	if err != nil {
		if resp != nil && resp.StatusCode == 403 {
			log.WithField("url", c.url).Warnf("dial forbidden: %s", resp.Status)
			return fmt.Errorf("err got: %s", resp.Status)
		}
		log.WithField("url", c.url).Warnf("dial error: %s", err.Error())
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		log.WithField("url", c.url).Warnf("unexpected status: %s", resp.Status)
		return fmt.Errorf("dial err: %s", resp.Status)
	}

	c.conn = conn
	log.WithField("url", c.url).Debug("dial succeeded")
	return nil
}

func (c *client) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.client.Close")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.conn == nil {
		return nil
	}
	log.Debug("closing connection")
	err := c.conn.Close()
	c.conn = nil
	return err
}

// isClosed reports whether Close has been called. ReadLoop uses it to stop
// reconnecting once the downstream client has disconnected.
func (c *client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *client) ReadLoop() {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.client.ReadLoop")
	for {
		// Snapshot conn under the lock: Close() may zero c.conn concurrently,
		// and reading it here without the lock was the source of both a
		// nil-pointer panic (Close races ReadLoop) and a data race on c.conn.
		c.mu.Lock()
		conn := c.conn
		closed := c.closed
		c.mu.Unlock()
		if closed || conn == nil {
			return
		}
		// Arm the read deadline to the earlier of the no-data watchdog and
		// the proactive refresh point (ExpireAt - refreshLead). A stalled
		// upstream that keeps the connection open but sends nothing trips the
		// watchdog; a URL about to expire triggers the refresh while data is
		// still flowing, so the client sees no interruption at all.
		if err := conn.SetReadDeadline(c.readDeadline()); err != nil {
			log.WithField("field", "set read deadline").Debugf("could not arm deadline: %s", err.Error())
		}
		mt, body, err := conn.ReadMessage()
		if err != nil {
			// Close() may have fired while we were blocked in ReadMessage; if
			// so the downstream client is gone and we must not reconnect.
			if c.isClosed() {
				c.pipe.CloseWithError(err)
				return
			}
			if c.extractFn != nil && (isRetriableWS(err) || isTimeout(err)) {
				reason := "watchdog"
				if isRetriableWS(err) {
					reason = "403"
				}
				log.Warnf("retriable websocket error (%s): %s, reconnecting...", reason, err.Error())
				if c.reconnect() {
					continue
				}
				// reconnect() closed the pipe (client gone or retries exhausted).
				return
			}
			c.pipe.CloseWithError(err)
			return
		}
		switch mt {
		case ws.BinaryMessage:
			if c.cacheKey != "" {
				if c.headerWriter == nil {
					// Wrap the pipe so the client sees which writes actually
					// reach it — the header cache writer may buffer data
					// indefinitely while detecting, and only real pipe writes
					// count as stream progress for the pipe-stall watchdog.
					c.headerWriter = flv.NewHeaderCacheWriter(&markingWriter{next: c.pipe, mark: c.markPipeWrite}, flv.DefaultCache, c.cacheKey)
				}
				if _, writeErr := c.headerWriter.Write(body); writeErr != nil {
					c.pipe.CloseWithError(writeErr)
					return
				}
			} else {
				c.pipe.Write(body)
				c.markPipeWrite()
			}
		case ws.TextMessage:
		case ws.CloseMessage:
			// Unreachable via gorilla's ReadMessage (close frames surface as
			// errors), kept for safety; err is nil here so wrap a real error.
			c.pipe.CloseWithError(fmt.Errorf("websocket close message received"))
			return
		default:
			c.pipe.CloseWithError(fmt.Errorf("unknown msg type: %d", mt))
			return
		}
	}
}

// readDeadline returns when the next ReadMessage must give up: the earliest
// of the message-silence watchdog, the pipe-stall watchdog (no consumable
// data reaching the pipe), and the proactive refresh point before URL
// expiry. A deadline in the past (stale ExpireAt) falls back to a capped
// slice of the watchdog so the read loop cannot spin on immediate timeouts.
func (c *client) readDeadline() time.Time {
	now := time.Now()
	deadline := now.Add(c.watchdogDuration())
	if c.previous != nil && c.previous.ExpireAt != nil {
		if refresh := c.previous.ExpireAt.Add(-c.refreshLeadDuration()); refresh.Before(deadline) {
			deadline = refresh
		}
	}
	if last := c.lastPipeWrite(); !last.IsZero() {
		if piped := last.Add(c.pipeWatchdogDuration()); piped.Before(deadline) {
			deadline = piped
		}
	}
	if deadline.After(now) {
		return deadline
	}
	fallback := c.watchdogDuration()
	if fallback > minReadDeadline {
		fallback = minReadDeadline
	}
	return now.Add(fallback)
}

// markingWriter wraps an io.Writer and notifies a callback whenever bytes
// actually pass through, so the client can track real data flow into the
// pipe behind the header cache writer.
type markingWriter struct {
	next io.Writer
	mark func()
}

func (m *markingWriter) Write(p []byte) (int, error) {
	n, err := m.next.Write(p)
	if n > 0 && m.mark != nil {
		m.mark()
	}
	return n, err
}

// reconnect replaces the current connection with a fresh one obtained via
// extractFn, retrying with backoff. It returns true when a new connection is
// up (the caller should continue reading); false when the client was closed
// or the retry budget was exhausted — in both cases the pipe is already
// closed with an error.
func (c *client) reconnect() bool {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.client.reconnect")
	// Drop the stale connection first so its socket doesn't linger.
	c.mu.Lock()
	oldConn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if oldConn != nil {
		oldConn.Close()
	}

	for fails := 0; ; fails++ {
		// Stop as soon as the downstream client is gone.
		if c.isClosed() {
			c.pipe.CloseWithError(errClientClosed)
			return false
		}

		result, extractErr := c.extractFn(c.previous)
		if extractErr != nil {
			log.WithField("field", "attempt").Warnf("extract for reconnect error (%d): %s", fails+1, extractErr.Error())
		} else if !isWebSocketURL(result.URL) {
			// The platform may stop serving ws for this room; treat as a
			// retryable failure in case the next extraction switches back.
			log.Warnf("extract returned non-websocket URL: %s", result.URL)
		} else {
			// DialContext reads c.url, so point it at the fresh URL first.
			c.url = result.URL
			ctx, cancel := context.WithTimeout(context.TODO(), dialTimeout)
			dialErr := c.DialContext(ctx)
			cancel()
			if dialErr == nil {
				c.previous = result
				// Reset header writer so the new stream's header is re-detected.
				c.headerWriter = nil
				// Give the fresh connection a full pipe-watchdog window to
				// deliver its first consumable data, instead of letting the
				// previous connection's stall immediately expire the deadline.
				c.markPipeWrite()
				log.WithField("field", "attempt").Infoln("reconnected successfully")
				return true
			}
			log.WithField("field", "attempt").Warnf("reconnect dial error (%d): %s", fails+1, dialErr.Error())
		}

		if fails+1 >= c.maxFails() {
			err := fmt.Errorf("reconnect failed after %d attempts", fails+1)
			log.Errorln(err.Error())
			c.pipe.CloseWithError(err)
			return false
		}
		time.Sleep(c.backoffAt(fails + 1))
	}
}

func (c *client) Read(b []byte) (int, error) {
	return c.pipe.Read(b)
}

func isRetriableWS(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "403")
}

// isTimeout reports whether err is a network timeout (e.g. the read watchdog
// deadline tripping), which means the connection went silent rather than
// broken — the upstream should be re-extracted and re-dialed.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isWebSocketURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme == "ws" || parsed.Scheme == "wss"
}
