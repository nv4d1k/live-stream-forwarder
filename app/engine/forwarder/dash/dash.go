package dash

import (
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// DASHForwarder builds DASHStreams with a proxied, UA-injecting HTTP client.
type DASHForwarder struct {
	proxy  *url.URL
	hc     *http.Client
	mobile bool
}

// NewDASHForwarder creates a DASH forwarder routed through the optional proxy.
func NewDASHForwarder(proxy *url.URL, mobile bool) *DASHForwarder {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.NewDASHForwarder")
	log.Debugf("creating DASHForwarder proxy=%v mobile=%v", proxy, mobile)
	d := &DASHForwarder{
		proxy:  proxy,
		hc:     &http.Client{},
		mobile: mobile,
	}
	if proxy != nil {
		d.hc.Transport = httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, mobile)
	} else {
		d.hc.Transport = httpweb.NewAddHeaderTransport(nil, mobile)
	}
	return d
}

// Stream returns a *DASHStream that continuously fetches the DASH manifest's
// representations and pipes an interleaved two-track fMP4 stream to the client.
func (d *DASHForwarder) Stream(extractFn stream.ExtractFunc) *DASHStream {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHForwarder.Stream")
	log.Debug("creating DASHStream from extractFn")
	return NewDASHStream(extractFn, d.hc)
}
