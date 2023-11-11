package hls

import (
	"encoding/base64"
	"fmt"
	"github.com/gin-gonic/gin"
	libm3u8 "github.com/grafov/m3u8"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type HLSForwarder struct {
	proxy *url.URL
	hc    *http.Client
}

func NewHLSForwarder(proxy *url.URL, mobile bool) *HLSForwarder {
	h := &HLSForwarder{
		proxy: proxy,
		hc:    &http.Client{},
	}
	if proxy != nil {
		h.hc.Transport = httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, mobile)
	} else {
		h.hc.Transport = httpweb.NewAddHeaderTransport(nil, mobile)
	}
	return h
}

func (m *HLSForwarder) WrapPlaylist(ctx *gin.Context, origin, prefix string) error {
	p := libm3u8.NewMasterPlaylist()
	ux, err := m.ConvertURL(origin, origin, prefix)
	if err != nil {
		return err
	}

	p.Append(ux, nil, libm3u8.VariantParams{
		Alternatives: []*libm3u8.Alternative{
			&libm3u8.Alternative{
				Type:       "VIDEO",
				Default:    true,
				Autoselect: "YES",
			},
		},
	})
	ctx.Writer.Header().Set("content-type", "application/vnd.apple.mpegurl")
	ctx.Writer.Header().Set("cache-control", "no-cache, no-store, private")
	ctx.Writer.Header().Set("transfer-encoding", "identity")
	ctx.String(200, p.Encode().String())
	return nil
}
func (m *HLSForwarder) ConvertURL(origin, last, prefix string) (string, error) {
	log := global.Log.WithField("function", "app.engine.forwarder.hls.ConvertURL")
	log.WithField("field", "origin url").Debug(origin)
	log.WithField("field", "last url").Debug(last)
	log.WithField("field", "prefix url").Debug(prefix)
	ou, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("parse item url in m3u8 error: %w", err)
	}
	lu, err := url.Parse(last)
	if err != nil {
		return "", fmt.Errorf("parse last access url error: %w", err)
	}
	u, err := url.Parse(prefix)
	if err != nil {
		return "", fmt.Errorf("parse prefix url error: %w", err)
	}
	if ou.Scheme == "" {
		ou.Scheme = lu.Scheme
	}
	if ou.Host == "" {
		ou.Host = lu.Host
	}
	if strings.Split(ou.Path, "/")[0] != "" {
		pa := strings.Split(lu.Path, "/")
		oa := pa[:len(pa)-1]
		oa = append(oa, ou.Path)
		ou.Path = strings.Join(oa, "/")
	}
	log.WithField("field", "converted url").Debug(ou.String())
	queryString := u.Query()
	if m.proxy != nil {
		queryString.Set("proxy", m.proxy.String())
	}
	oue := base64.StdEncoding.EncodeToString([]byte(ou.String()))
	log.WithField("field", "origin url after encoded").Debug(oue)
	queryString.Set("url", oue)
	u.RawQuery = queryString.Encode()
	log.WithField("field", "url after converted").Debug(u.String())
	return u.String(), nil
}

func (m *HLSForwarder) ForwardM3u8(ctx *gin.Context, url, prefix string) error {
	log := global.Log.WithField("function", "app.engine.forwarder.hls.ForwardM3u8")
	p, lt, err := m.GetM3u8(url)
	if err != nil {
		return fmt.Errorf("get m3u8 error: %w", err)
	}
	switch lt {
	case libm3u8.MEDIA:
		mediapl := p.(*libm3u8.MediaPlaylist)
		if mediapl.Map != nil && mediapl.Map.URI != "" {
			xu, err := m.ConvertURL(mediapl.Map.URI, url, prefix)
			if err != nil {
				return fmt.Errorf("convert map url error: %w", err)
			}
			mediapl.Map.URI = xu
		}
		for _, u := range mediapl.Segments {
			if u == nil {
				continue
			}
			v, err := m.ConvertURL(u.URI, url, prefix)
			if err != nil {
				return fmt.Errorf("convert url error: %w", err)
			}
			u.URI = v
		}
		mediapl.ResetCache()
	case libm3u8.MASTER:
		masterpl := p.(*libm3u8.MasterPlaylist)
		for _, v := range masterpl.Variants {
			u, err := m.ConvertURL(v.URI, url, prefix)
			if err != nil {
				return fmt.Errorf("convert url error: %w", err)
			}
			v.URI = u
		}
		masterpl.ResetCache()
	}
	ctx.Writer.Header().Set("content-type", "application/vnd.apple.mpegurl")
	ctx.Writer.Header().Set("cache-control", "no-cache, no-store, private")
	ctx.Writer.Header().Set("transfer-encoding", "identity")
	log.WithField("field", "m3u8 write to client").Debug(p.Encode().String())

	ctx.String(200, p.Encode().String())
	return nil
}

func (m *HLSForwarder) GetM3u8(url string) (libm3u8.Playlist, libm3u8.ListType, error) {
	log := global.Log.WithField("function", "app.engine.forwarder.hls.GetM3u8")
	resp, err := m.hc.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("get m3u8 file error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("err got: %s", resp.Status)
	}

	p, lt, err := libm3u8.DecodeFrom(resp.Body, true)
	if err != nil {
		return nil, 0, fmt.Errorf("decode m3u8 file error: %w", err)
	}
	log.WithField("field", "origin m3u8").Debug(p.Encode().String())
	return p, lt, err

}

func (m *HLSForwarder) Forward(ctx *gin.Context, uu, prefix string) error {
	log := global.Log.WithField("function", "app.engine.forwarder.hls.Forward")
	rawb, err := base64.StdEncoding.DecodeString(uu)
	rawu := string(rawb)
	if err != nil {
		return fmt.Errorf("decoding url error: %w", err)
	}
	log.WithField("field", "backend url").Debug(rawu)
	ux, err := url.Parse(rawu)
	if err != nil {
		return err
	}
	ff := strings.Split(ux.Path, ".")
	if ff[len(ff)-1] == "m3u8" {
		return m.ForwardM3u8(ctx, rawu, prefix)
	} else {
		resp, err := m.hc.Get(rawu)
		if err != nil {
			return fmt.Errorf("get backend file error: %w", err)
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("err got: %s", resp.Status)
		}
		defer resp.Body.Close()
		headers := resp.Header
		ctx.Status(resp.StatusCode)
		for hk, hv := range headers {
			ctx.Header(hk, strings.Join(hv, ";"))
		}
		ctx.Writer.Header().Set("transfer-encoding", "identity")
		ctx.Writer.Flush()
		_, err = io.Copy(ctx.Writer, resp.Body)
		if err != nil {
			return fmt.Errorf("copy chunks error: %w", err)
		}
	}
	return nil
}
