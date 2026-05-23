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
	mobile    bool
	extractFn stream.ExtractFunc
	cacheKey  string
}

func NewWebSocketForwarder(proxy *url.URL, mobile bool) Foreground {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewWebSocketForwarder")
	log.WithField("mobile", mobile).Debug("creating WebSocketForwarder")
	return &WebSocketForwarder{
		stopCh: make(chan struct{}),
		proxy:  proxy,
		mobile: mobile,
	}
}

// NewWebSocketForwarderWithRetry creates a forwarder that will reconnect
// using extractFn when the upstream connection fails with a retriable error.
// cacheKey enables FLV header caching; empty string disables it.
func NewWebSocketForwarderWithRetry(proxy *url.URL, mobile bool, extractFn stream.ExtractFunc, cacheKey string) Foreground {
	log := global.Log.WithField("func", "app.engine.forwarder.websocket.NewWebSocketForwarderWithRetry")
	log.WithField("mobile", mobile).WithField("cacheKey", cacheKey).Debug("creating WebSocketForwarderWithRetry")
	return &WebSocketForwarder{
		stopCh:    make(chan struct{}),
		proxy:     proxy,
		mobile:    mobile,
		extractFn: extractFn,
		cacheKey:  cacheKey,
	}
}

func (s *WebSocketForwarder) httpHeader() http.Header {
	h := make(http.Header)
	h.Set("User-Agent", global.DEFAULT_USER_AGENT)
	if s.mobile {
		h.Set("User-Agent", global.DEFAULT_MOBILE_USER_AGENT)
	}
	return h
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
	buffer := bytes.NewBuffer(nil)
	buffer.WriteString("HTTP/1.1 200 OK\r\n")
	buffer.WriteString("Content-Type: video/x-flv\r\n")
	buffer.WriteString("Transfer-Encoding: identity\r\n")
	buffer.WriteString("Connection: close\r\n")
	buffer.WriteString("Cache-Control: no-cache\r\n")
	buffer.WriteString("Access-Control-Allow-Origin: *\r\n")
	buffer.WriteString("Access-Control-Allow-Headers: *\r\n")
	buffer.WriteString("Access-Control-Allow-Methods: *\r\n")
	buffer.WriteString("\r\n")

	_, err = conn.Write(buffer.Bytes())
	if err != nil {
		log.WithError(err).Errorln("write frontend error")
		return err
	}

	// Send cached FLV header to the client if available.
	if s.cacheKey != "" {
		entry := flv.DefaultCache.GetOrCreate(s.cacheKey)
		entry.Wait()
		if data := entry.Data(); data != nil {
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
