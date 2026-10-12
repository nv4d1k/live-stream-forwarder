package httpweb

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
)

// TestStreamTLSConfigKeepsRSAStaticSuites pins the regression: DouYu's scdn
// edge fleet (edgesrv.com) runs TLS stacks that only complete handshakes
// when a static-RSA cipher suite is offered — modern Go removed TLS_RSA_*
// from the client defaults (no forward secrecy), so every http stream fetch
// to those edges died with "remote error: tls: handshake failure". The
// config must also keep verification on: the edges present valid
// *.edgesrv.com certificates, and the ws dialer already carries the same
// suites for the same reason.
func TestStreamTLSConfigKeepsRSAStaticSuites(t *testing.T) {
	cfg := StreamTLSConfig()
	if cfg == nil {
		t.Fatal("StreamTLSConfig returned nil")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must stay off: stream edges present verifiable certificates")
	}
	got := make(map[uint16]bool)
	for _, c := range cfg.CipherSuites {
		got[c] = true
	}
	if !got[tls.TLS_RSA_WITH_AES_256_GCM_SHA384] || !got[tls.TLS_RSA_WITH_AES_128_GCM_SHA256] {
		t.Error("static RSA suites missing: scdn edges reject ClientHellos without them")
	}
	if !got[tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256] || !got[tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384] {
		t.Error("ECDHE suites missing: modern CDNs must keep negotiating forward-secret suites")
	}
	if cfg.MinVersion != 0 || cfg.MaxVersion != 0 {
		t.Error("TLS version bounds must stay at Go defaults")
	}
}

// TestNewStreamTransportWiresProxyAndTLS verifies the shared stream transport
// carries the proxy and the CDN TLS config in both proxy and direct modes.
func TestNewStreamTransportWiresProxyAndTLS(t *testing.T) {
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}

	p := &url.URL{Scheme: "http", Host: "10.0.0.1:1080"}
	tr := NewStreamTransport(p)
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("proxy call: %v", err)
	}
	if got == nil || got.String() != p.String() {
		t.Errorf("proxy not wired: got %v, want %v", got, p)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig missing")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("TLSClientConfig must verify certificates")
	}

	direct := NewStreamTransport(nil)
	got, err = direct.Proxy(req)
	if err != nil {
		t.Fatalf("proxy call: %v", err)
	}
	if got != nil {
		t.Errorf("nil proxy must yield no proxy, got %v", got)
	}
	if direct.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig missing in direct mode (no-proxy streams hit the same CDN edges)")
	}
}

// TestNewStreamTransportBoundsStalledPhases pins the transport timeouts: a
// custom http.Transport with zero DialContext/TLSHandshakeTimeout/
// ResponseHeaderTimeout never times out, so a CDN edge that accepts the TCP
// connection but never answers hangs the fetching goroutine forever — the
// 2026-10-12 dead-edge incident on the ws→flv fallback path. The timeouts
// bound everything up to the response headers only; the streamed body is a
// live stream and must never be bounded by a client timeout.
func TestNewStreamTransportBoundsStalledPhases(t *testing.T) {
	tr := NewStreamTransport(nil)
	if tr.DialContext == nil {
		t.Error("DialContext missing: a stalled TCP connect would hang forever")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout missing: a stalled TLS handshake would hang forever")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout missing: an edge that never answers headers would hang forever")
	}
}
