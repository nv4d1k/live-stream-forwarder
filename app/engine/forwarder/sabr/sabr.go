package sabr

import (
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/fanout"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// DefaultHub is the process-wide fan-out hub used by the HTTP layer for
// SABR streams: one upstream core per platform:room|proxy|quality, shared
// by every client of that room and quality. YouTube binds its innertube
// URLs to the proxy exit IP, so sessions through different proxies cannot
// share one upstream stream.
var DefaultHub = fanout.NewHub()

// SABRForwarder builds SABRStreams with a proxied, UA-injecting HTTP client.
type SABRForwarder struct {
	proxy     *url.URL
	hc        *http.Client
	userAgent string
}

// NewSABRForwarder creates a SABR forwarder routed through the optional proxy.
func NewSABRForwarder(proxy *url.URL, userAgent string) *SABRForwarder {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.NewSABRForwarder")
	log.Debugf("creating SABRForwarder proxy=%v userAgent=%q", proxy, userAgent)
	d := &SABRForwarder{
		proxy:     proxy,
		hc:        &http.Client{},
		userAgent: userAgent,
	}
	if proxy != nil {
		d.hc.Transport = httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, userAgent)
	} else {
		d.hc.Transport = httpweb.NewAddHeaderTransport(nil, userAgent)
	}
	return d
}

// Stream returns a *SABRStream that continuously requests the SABR endpoint
// and pipes an interleaved two-track fMP4 stream to the client.
func (d *SABRForwarder) Stream(extractFn stream.ExtractFunc) *SABRStream {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.SABRForwarder.Stream")
	log.Debug("creating SABRStream from extractFn")
	return NewSABRStream(extractFn, d.hc)
}
