package hls

import (
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
)

type HLSForwarder struct {
	proxy  *url.URL
	hc     *http.Client
	mobile bool
}

func NewHLSForwarder(proxy *url.URL, mobile bool) *HLSForwarder {
	h := &HLSForwarder{
		proxy:  proxy,
		hc:     &http.Client{},
		mobile: mobile,
	}
	if proxy != nil {
		h.hc.Transport = httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, mobile)
	} else {
		h.hc.Transport = httpweb.NewAddHeaderTransport(nil, mobile)
	}
	return h
}

// Stream returns an *HLSStream that continuously fetches the HLS playlist,
// downloads segments, and pipes raw MPEG-TS data to the client.
func (h *HLSForwarder) Stream(extractFn stream.ExtractFunc) *HLSStream {
	return NewHLSStream(extractFn, h.hc)
}
