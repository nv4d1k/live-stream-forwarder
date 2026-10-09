package hls

import (
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

type HLSForwarder struct {
	proxy     *url.URL
	hc        *http.Client
	userAgent string
}

func NewHLSForwarder(proxy *url.URL, userAgent string) *HLSForwarder {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.NewHLSForwarder")
	log.Debugf("creating HLSForwarder proxy=%v userAgent=%q", proxy, userAgent)
	h := &HLSForwarder{
		proxy:     proxy,
		hc:        &http.Client{},
		userAgent: userAgent,
	}
	if proxy != nil {
		h.hc.Transport = httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, userAgent)
	} else {
		h.hc.Transport = httpweb.NewAddHeaderTransport(nil, userAgent)
	}
	return h
}

// Stream returns an *HLSStream that continuously fetches the HLS playlist,
// downloads segments, and pipes raw MPEG-TS data to the client.
func (h *HLSForwarder) Stream(extractFn stream.ExtractFunc) *HLSStream {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.HLSForwarder.Stream")
	log.Debug("creating HLSStream from extractFn")
	return NewHLSStream(extractFn, h.hc)
}
