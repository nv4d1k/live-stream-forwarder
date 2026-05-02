package websocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/flv"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
	ws "github.com/gorilla/websocket"
)

func NewXP2PClient(u string, header http.Header, proxy *url.URL) Background {
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
	stopCh       chan struct{}
	pipe         *stream.Pipe
	extractFn    stream.ExtractFunc
	previous     *stream.ExtractResult
	cacheKey     string
	headerWriter *flv.HeaderCacheWriter
}

func (c *client) Start() error {
	log := global.Log.WithField("function", "app.engine.forwarder.websocket.client.Start")
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := c.DialContext(ctx)
	if err != nil {
		return fmt.Errorf("dial context error: %w", err)
	}
	go c.ReadLoop()
	return nil
}

func (c *client) DialContext(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, resp, err := c.dialer.DialContext(ctx, c.url, c.header)
	if err != nil {
		if resp != nil && resp.StatusCode == 403 {
			return fmt.Errorf("err got: %s", resp.Status)
		}
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("dial err: %s", resp.Status)
	}

	c.conn = conn
	return nil
}

func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *client) ReadLoop() {
	log := global.Log.WithField("function", "app.engine.forwarder.websocket.client.ReadLoop")
	for {
		mt, body, err := c.conn.ReadMessage()
		if err != nil {
			if c.extractFn != nil && isRetriableWS(err) {
				log.Warnf("retriable websocket error: %s, reconnecting...", err.Error())
				c.conn.Close()
				result, extractErr := c.extractFn(c.previous)
				if extractErr != nil {
					log.Errorf("extract for reconnect error: %s", extractErr.Error())
					c.pipe.CloseWithError(err)
					return
				}
				// Validate that the re-extracted URL is still a websocket URL
				if !isWebSocketURL(result.URL) {
					log.Warnf("extract returned non-websocket URL: %s, retrying", result.URL)
					c.pipe.CloseWithError(fmt.Errorf("extract returned non-websocket URL on retry"))
					return
				}
				c.url = result.URL
				c.previous = result
				// Reset header writer so the new stream's header is re-detected.
				c.headerWriter = nil
				c.mu.Lock()
				conn, resp, dialErr := c.dialer.DialContext(context.TODO(), c.url, c.header)
				c.mu.Unlock()
				if dialErr != nil {
					log.Errorf("reconnect dial error: %s", dialErr.Error())
					c.pipe.CloseWithError(dialErr)
					return
				}
				if resp.StatusCode != http.StatusSwitchingProtocols {
					c.pipe.CloseWithError(fmt.Errorf("reconnect dial err: %s", resp.Status))
					return
				}
				c.mu.Lock()
				c.conn = conn
				c.mu.Unlock()
				continue
			}
			c.pipe.CloseWithError(err)
			return
		}
		switch mt {
		case ws.BinaryMessage:
			if c.cacheKey != "" {
				if c.headerWriter == nil {
					c.headerWriter = flv.NewHeaderCacheWriter(c.pipe, flv.DefaultCache, c.cacheKey)
				}
				if _, writeErr := c.headerWriter.Write(body); writeErr != nil {
					c.pipe.CloseWithError(writeErr)
					return
				}
			} else {
				c.pipe.Write(body)
			}
		case ws.TextMessage:
		case ws.CloseMessage:
			c.pipe.CloseWithError(err)
			return
		default:
			c.pipe.CloseWithError(fmt.Errorf("unknown msg type: %d", mt))
			return
		}
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

func isWebSocketURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme == "ws" || parsed.Scheme == "wss"
}
