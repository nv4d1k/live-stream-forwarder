package httpweb

import (
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"io"
	"net/http"
	"net/url"
)

type HTTPWebForwarder struct {
	Client *http.Client
}

func NewHTTPWebForwarder(proxy *url.URL, mobile bool) *HTTPWebForwarder {
	h := new(HTTPWebForwarder)
	h.Client = &http.Client{}
	h.Client.Transport = NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, mobile)
	return h
}

func (h *HTTPWebForwarder) Forward(ctx *gin.Context, headers http.Header, u string, depth int) error {
	log := global.Log.WithField("function", "app.engine.forwarder.httpweb.HTTPWebForwarder.Forward")
	log.WithField("field", "backend url").Debug(u)
	if depth > 10 {
		return errors.New("too many redirections")
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return fmt.Errorf("making backend request error: %w\n", err)
	}
	for hk, _ := range headers {
		req.Header.Set(hk, headers.Get(hk))
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("sending backend request error: %w\n", err)
	}
	defer resp.Body.Close()
	log.WithField("field", "backend request headers").Debugf("%v", resp.Request.Header)
	switch resp.StatusCode {
	case 200:
		respheaders := resp.Header
		ctx.Status(resp.StatusCode)
		for hk, _ := range respheaders {
			ctx.Header(hk, respheaders.Get(hk))
		}
		//ctx.Writer.Header().Set("Transfer-Encoding", "identity")
		ctx.Writer.Flush()
		_, err = io.Copy(ctx.Writer, resp.Body)
		if err != nil {
			return fmt.Errorf("copy chunks error: %w", err)
		}
	case 301, 302:
		l := resp.Header.Get("Location")
		if l == "" {
			return fmt.Errorf("err no redirect location")
		}
		_, err = url.Parse(l)
		if err != nil {
			return fmt.Errorf("err url in location")
		}
		return h.Forward(ctx, headers, l, depth+1)
	default:
		return fmt.Errorf("err got: %s", resp.Status)
	}
	return nil
}
