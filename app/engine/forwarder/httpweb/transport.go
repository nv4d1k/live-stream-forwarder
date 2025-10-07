package httpweb

import (
	"net/http"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

type AddHeaderTransport struct {
	T   http.RoundTripper
	mob bool
}

func (adt *AddHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ua := global.DEFAULT_USER_AGENT
	if adt.mob {
		ua = global.DEFAULT_MOBILE_USER_AGENT
	}
	req.Header.Add("User-Agent", ua)
	return adt.T.RoundTrip(req)
}

func NewAddHeaderTransport(T http.RoundTripper, mobile bool) *AddHeaderTransport {
	if T == nil {
		T = http.DefaultTransport
	}
	return &AddHeaderTransport{T, mobile}
}
