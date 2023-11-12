package controllers

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/BiliBili"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYu"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/HuYa"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/Twitch"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/hls"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/websocket"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func Forwarder(c *gin.Context) {
	log := global.Log.WithField("function", "app.http.controllers.Forwarder")
	proxy := c.GetString("proxy")
	var proxyURL *url.URL
	var err error
	if proxy != "" {
		proxyURL, err = url.Parse(proxy)
		if err != nil {
			log.Errorf("parsing proxy error: %s\n", err.Error())
			c.String(400, "invalid proxy")
			return
		}
	}

	log.WithField("field", "url path").Debug(c.Request.URL.Path)
	log.WithField("field", "room").Debugf("%s %s\n", c.Param("platform"), c.Param("room"))
	log.WithField("field", "query").Debug(c.Request.URL.RawQuery)
	if proxyURL != nil {
		log.WithField("field", "proxy").Debug(proxyURL.String())
	}
	log = log.WithField("platform", strings.ToLower(c.Param("platform"))).WithField("room", c.Param("room"))
	switch strings.ToLower(c.Param("platform")) {
	case "douyu":
		dy, err := DouYu.NewDouyuLink(c.Param("room"), proxyURL)
		if err != nil {
			log.Errorf("create link object error: %s\n", err.Error())
			c.String(400, err.Error())
			return
		}
		u, err := dy.GetLink()
		if err != nil {
			log.Errorf("get link error: %s\n", err.Error())
			c.String(400, err.Error())
			return
		}
		switch u.Scheme {
		case "http", "https":
			f := httpweb.NewHTTPWebForwarder(proxyURL, false)
			err = f.Forward(c, make(http.Header), u.String(), 0)
			if err != nil {
				log.Errorf("forward http(s) stream error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
		case "ws", "wss":
			f := websocket.NewWebSocketForwarder(proxyURL, false)
			err = f.Start(c, u.String())
			if err != nil {
				log.Errorf("forward ws(s) stream error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
		default:
			c.String(500, "unsupported schema")
			return
		}
		return

	case "huya":
		pp := c.DefaultQuery("url", "")
		prefix := fmt.Sprintf("%s://%s%s", func() string {
			if c.Request.TLS != nil {
				return "https"
			}
			return "http"
		}(), c.Request.Host, c.Request.URL.Path)
		if pp == "" {
			link, err := HuYa.NewHuyaLink(c.Param("room"), proxyURL)
			if err != nil {
				log.Errorf("create link object error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			u, err := link.GetLink()
			if err != nil {
				log.Errorf("get link error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			switch path.Ext(u.Path) {
			case ".m3u8":
				p := hls.NewHLSForwarder(proxyURL, true)
				err := p.WrapPlaylist(c, u.String(), prefix)
				if err != nil {
					log.Errorf("wrap play list error: %s\n", err.Error())
					c.String(400, err.Error())
					return
				}
			case ".flv":
				p := httpweb.NewHTTPWebForwarder(proxyURL, true)
				err = p.Forward(c, make(http.Header), u.String(), 0)
				if err != nil {
					log.Errorf("forward http(s) stream error: %s\n", err.Error())
					c.String(500, err.Error())
					return
				}
			default:
				c.String(500, "unsupported format")
				return
			}
		} else {
			p := hls.NewHLSForwarder(proxyURL, true)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		}
		return

	case "twitch":
		pp := c.DefaultQuery("url", "")
		prefix := fmt.Sprintf("%s://%s%s", func() string {
			if c.Request.TLS != nil {
				return "https"
			}
			return "http"
		}(), c.Request.Host, c.Request.URL.Path)
		if pp == "" {
			link, err := Twitch.NewTwitchLink(c.Param("room"), proxyURL)
			if err != nil {
				log.Errorf("create link object error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			url, err := link.GetLink()
			if err != nil {
				log.Errorf("get link error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			p := hls.NewHLSForwarder(proxyURL, false)
			err = p.ForwardM3u8(c, url.String(), prefix)
			if err != nil {
				log.Errorf("forward playlist error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		} else {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		}
		return

	case "bilibili":
		pp := c.DefaultQuery("url", "")
		prefix := fmt.Sprintf("%s://%s%s", func() string {
			if c.Request.TLS != nil {
				return "https"
			}
			return "http"
		}(), c.Request.Host, c.Request.URL.Path)
		headers := make(http.Header)
		headers.Set("Referer", "https://live.bilibili.com")
		if pp == "" {
			link, err := BiliBili.NewBiliBiliLink(c.Param("room"), proxyURL)
			if err != nil {
				log.Errorf("create link object error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			u, err := link.GetLink()
			if err != nil {
				log.Errorf("get link error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			switch path.Ext(u.Path) {
			case ".m3u8":
				p := hls.NewHLSForwarder(proxyURL, false)
				err := p.WrapPlaylist(c, u.String(), prefix)
				if err != nil {
					log.Errorf("wrap play list error: %s\n", err.Error())
					c.String(400, err.Error())
					return
				}
			case ".flv":
				p := httpweb.NewHTTPWebForwarder(proxyURL, false)
				err = p.Forward(c, headers, u.String(), 0)
				if err != nil {
					log.Errorf("forward http(s) stream error: %s\n", err.Error())
					c.String(500, err.Error())
					return
				}
			default:
				c.String(500, "unsupported format")
				return
			}
		} else {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		}
		return

	case "douyin":
		pp := c.DefaultQuery("url", "")
		prefix := fmt.Sprintf("%s://%s%s", func() string {
			if c.Request.TLS != nil {
				return "https"
			}
			return "http"
		}(), c.Request.Host, c.Request.URL.Path)
		if pp == "" {
			link, err := DouYin.NewDouYinLink(c.Param("room"), proxyURL)
			if err != nil {
				log.Errorf("create link object error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			u, err := link.GetLink()
			if err != nil {
				log.Errorf("get link error: %s\n", err.Error())
				c.String(500, err.Error())
				return
			}
			switch path.Ext(u.Path) {
			case ".m3u8":
				p := hls.NewHLSForwarder(proxyURL, false)
				err := p.WrapPlaylist(c, u.String(), prefix)
				if err != nil {
					log.Errorf("wrap play list error: %s\n", err.Error())
					c.String(400, err.Error())
					return
				}
			case ".flv":
				p := httpweb.NewHTTPWebForwarder(proxyURL, false)
				err = p.Forward(c, make(http.Header), u.String(), 0)
				if err != nil {
					log.Errorf("forward http(s) stream error: %s\n", err.Error())
					c.String(500, err.Error())
					return
				}
			default:
				c.String(500, "unsupported format")
				return
			}
		} else {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		}
		return

	default:
		c.String(400, "unsupported platform")
		return
	}
}
