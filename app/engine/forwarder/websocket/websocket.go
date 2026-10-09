package websocket

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/flv"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

type WebSocketForwarder struct {
	stopCh    chan struct{}
	proxy     *url.URL
	userAgent string
	extractFn stream.ExtractFunc
	cacheKey  string
}

func NewWebSocketForwarder(proxy *url.URL, userAgent string) Foreground {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewWebSocketForwarder")
	log.WithField("userAgent", userAgent).Debug("creating WebSocketForwarder")
	return &WebSocketForwarder{
		stopCh:    make(chan struct{}),
		proxy:     proxy,
		userAgent: userAgent,
	}
}

// NewWebSocketForwarderWithRetry creates a forwarder that will reconnect
// using extractFn when the upstream connection fails with a retriable error.
// cacheKey enables FLV header caching; empty string disables it.
func NewWebSocketForwarderWithRetry(proxy *url.URL, userAgent string, extractFn stream.ExtractFunc, cacheKey string) Foreground {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewWebSocketForwarderWithRetry")
	log.WithField("userAgent", userAgent).WithField("cacheKey", cacheKey).Debug("creating WebSocketForwarderWithRetry")
	return &WebSocketForwarder{
		stopCh:    make(chan struct{}),
		proxy:     proxy,
		userAgent: userAgent,
		extractFn: extractFn,
		cacheKey:  cacheKey,
	}
}

func (s *WebSocketForwarder) httpHeader() http.Header {
	ua := s.userAgent
	if ua == "" {
		ua = global.DEFAULT_USER_AGENT
	}
	h := make(http.Header)
	h.Set("User-Agent", ua)
	return h
}

// frontendResponseHeader builds the HTTP/1.1 response header sent to the
// downstream client after hijacking the connection. The body is the live FLV
// stream, framed as a close-delimited body (no Content-Length).
//
// Note: "Transfer-Encoding: identity" is deliberately NOT sent. HTTP/2 has no
// notion of Transfer-Encoding (it is a hop-by-hop HTTP/1.1 construct), so a
// reverse proxy that speaks HTTP/2 to the client (e.g. Caddy) fails to
// translate an "identity"-encoded HTTP/1.1 upstream and returns 502. A
// close-delimited body (Connection: close, no Content-Length) is the
// standard, proxy-friendly way to stream an indefinite response.
func frontendResponseHeader() []byte {
	buffer := bytes.NewBuffer(nil)
	buffer.WriteString("HTTP/1.1 200 OK\r\n")
	buffer.WriteString("Content-Type: video/x-flv\r\n")
	buffer.WriteString("Connection: close\r\n")
	buffer.WriteString("Cache-Control: no-cache\r\n")
	buffer.WriteString("Access-Control-Allow-Origin: *\r\n")
	buffer.WriteString("Access-Control-Allow-Headers: *\r\n")
	buffer.WriteString("Access-Control-Allow-Methods: *\r\n")
	buffer.WriteString("\r\n")
	return buffer.Bytes()
}

func (s *WebSocketForwarder) Start(c *gin.Context, u string) error {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.WebSocketForwarder.Start")
	log.WithField("field", "backend url").Debug(u)
	ux, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("parse backend url error: %w", err)
	}
	var st Background
	switch ux.Scheme {
	case "ws", "wss":
		if s.extractFn != nil {
			st = NewXP2PClientWithRetry(s.extractFn, s.httpHeader(), s.proxy, s.cacheKey)
		} else {
			st = NewXP2PClient(u, s.httpHeader(), s.proxy)
		}
	default:
		return fmt.Errorf("unknown protocol: %s", ux.Scheme)
	}
	err = st.Start()
	if err != nil {
		log.Errorln("start backend error:", err.Error())
		return err
	}
	w := c.Writer

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Errorln(err.Error())
		if conn != nil {
			conn.Close()
		}
		return err
	}
	if _, err := conn.Write(frontendResponseHeader()); err != nil {
		log.WithError(err).Errorln("write frontend error")
		return err
	}

	// Send cached FLV header to the client if available. The pipe carries
	// media data only (HeaderCacheWriter strips headers), so the header must
	// come from the cache.
	if s.cacheKey != "" {
		entry := flv.DefaultCache.GetOrCreate(s.cacheKey)
		if !entry.WaitTimeout(flv.HeaderWaitTimeout) {
			log.WithField("field", "cache key").WithField("key", s.cacheKey).Warnf("timeout waiting for FLV header after %s, streaming without header", flv.HeaderWaitTimeout)
		} else if data := entry.Data(); len(data) > 0 {
			if _, writeErr := conn.Write(data); writeErr != nil {
				log.WithError(writeErr).Errorln("write cached header error")
				st.Close()
				conn.Close()
				return writeErr
			}
		}
	}

	go func() {
		defer st.Close()
		defer conn.Close()
		for {
			buf := make([]byte, 65536)
			n, err := st.Read(buf)
			if err != nil {
				return
			}
			_, err = conn.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	return nil
}
