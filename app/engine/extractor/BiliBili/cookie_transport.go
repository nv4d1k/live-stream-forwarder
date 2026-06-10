package BiliBili

import "net/http"

type cookieTransport struct {
	base   http.RoundTripper
	cookie string
}

func newCookieTransport(base http.RoundTripper, cookie string) *cookieTransport {
	return &cookieTransport{base: base, cookie: cookie}
}

func (ct *cookieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if ct.cookie != "" && (req.Host == "api.live.bilibili.com" || req.Host == "live.bilibili.com") {
		req.Header.Set("Cookie", ct.cookie)
	}
	return ct.base.RoundTrip(req)
}
