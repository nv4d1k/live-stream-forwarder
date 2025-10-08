package websocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	ws "github.com/gorilla/websocket"
)

func NewXP2PClient(u string, header http.Header, proxy *url.URL) Background {
	c := &client{
		url:    u,
		header: header,
		dialer: &ws.Dialer{
			// Configure TLS settings to handle handshake failures
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,             // Skip certificate verification (use with caution)
				MinVersion:         tls.VersionTLS10, // Support older TLS versions
				MaxVersion:         tls.VersionTLS13, // Support latest TLS version
				CurvePreferences: []tls.CurveID{
					tls.CurveP256,
					tls.X25519, // This curve is preferred
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
		pipe: NewPipe(),
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
	mu     sync.Mutex
	url    string
	header http.Header
	dialer *ws.Dialer
	conn   *ws.Conn
	stopCh chan struct{}
	pipe   *Pipe
}

func (c *client) Start() error {
	ctx := context.TODO()
	// Set context timeout longer than handshake timeout
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
	for {
		mt, body, err := c.conn.ReadMessage()
		if err != nil {
			c.pipe.CloseWithError(err)
			return
		}
		switch mt {
		case ws.BinaryMessage:
			c.pipe.Write(body)
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
