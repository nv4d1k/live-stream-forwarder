package websocket

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"net/http"
	"net/url"
)

type WebSocketForwarder struct {
	stopCh chan struct{}
	proxy  *url.URL
	mobile bool
}

func NewWebSocketForwarder(proxy *url.URL, mobile bool) Foreground {
	return &WebSocketForwarder{
		stopCh: make(chan struct{}),
		proxy:  proxy,
		mobile: mobile,
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
	log := global.Log.WithField("function", "app.engine.forwarder.websocket.WebSocketForwarder.Start")
	log.WithField("field", "backend url").Debug(u)
	ux, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("parse backend url error: %w", err)
	}
	var st Background
	switch ux.Scheme {
	case "ws", "wss":
		st = NewXP2PClient(u, s.httpHeader(), s.proxy)
	default:
		return fmt.Errorf("unknown protocol: %s", ux.Scheme)
	}
	err = st.Start()
	if err != nil {
		return err
	}
	w := c.Writer
	w.Header().Set("Content-Type", "video/x-flv")
	w.Header().Set("Transfer-Encoding", "identity")
	w.Header().Set("Connection", "close")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.WriteHeader(200)
	w.Flush()

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		conn.Close()
		return err
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
