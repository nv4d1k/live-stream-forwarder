package httpweb

import (
	"net/http"

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
