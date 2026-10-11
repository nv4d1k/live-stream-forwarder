package httpweb

import (
	"crypto/tls"
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// AddHeaderTransport injects a User-Agent into every request. An empty
// userAgent falls back to the default desktop User-Agent.
type AddHeaderTransport struct {
	T  http.RoundTripper
	ua string
}

func (adt *AddHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ua := adt.ua
	if ua == "" {
		ua = global.DEFAULT_USER_AGENT
	}
	req.Header.Add("User-Agent", ua)
	return adt.T.RoundTrip(req)
}

// NewAddHeaderTransport wraps T with User-Agent injection. userAgent may be
// empty for the default desktop User-Agent.
func NewAddHeaderTransport(T http.RoundTripper, userAgent string) *AddHeaderTransport {
	log := global.Log.WithField("func", "app.engine.forwarder.httpweb.NewAddHeaderTransport")
	log.Debugln("creating AddHeaderTransport")
	if T == nil {
		T = http.DefaultTransport
	}
	return &AddHeaderTransport{T, userAgent}
}

// StreamTLSConfig returns the TLS configuration for upstream stream fetches.
// It is Go's client default cipher list plus the two static-RSA suites:
// DouYu's scdn edge fleet (edgesrv.com) runs TLS stacks that answer a
// ClientHello without a static-RSA suite with "handshake failure", and
// modern Go removed TLS_RSA_* from the defaults (no forward secrecy), which
// broke every http stream fetch to those edges. The RSA suites are only
// negotiated when the server refuses everything else — modern CDNs keep
// their ECDHE preference. Certificate verification stays on: the edges
// present valid *.edgesrv.com certificates. The XP2P ws dialer has carried
// the same suites for the same reason since it was written.
func StreamTLSConfig() *tls.Config {
	return &tls.Config{
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
	}
}

// NewStreamTransport builds the http.Transport shared by the stream
// forwarders (FLV, HLS and the websocket forwarder's http-flv fallback):
// proxy-aware and carrying StreamTLSConfig so CDN edges with legacy TLS
// stacks stay reachable.
func NewStreamTransport(proxy *url.URL) *http.Transport {
	log := global.Log.WithField("func", "app.engine.forwarder.httpweb.NewStreamTransport")
	log.WithField("field", "proxy").Debugln("creating stream transport")
	return &http.Transport{
		Proxy:           http.ProxyURL(proxy),
		TLSClientConfig: StreamTLSConfig(),
	}
}
