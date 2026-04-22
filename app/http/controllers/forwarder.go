package controllers

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/BiliBili"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYu"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/HuYa"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/Twitch"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/hls"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/websocket"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// buildPrefix constructs the self-referencing URL prefix for HLS URL rewriting.
func buildPrefix(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.Request.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, c.Request.URL.Path)
}

// streamToClient writes data from a *stream.Stream to the gin context as a
// long-lived HTTP stream (FLV or raw binary). The client sees a single
// continuous response even if the producer reconnects on 403.
func streamToClient(c *gin.Context, s *stream.Stream, contentType string) {
	c.Writer.Header().Set("Content-Type", contentType)
	c.Writer.Header().Set("Transfer-Encoding", "identity")
	c.Writer.Header().Set("Connection", "close")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
	c.Writer.Header().Set("Access-Control-Allow-Headers", "*")
	c.Writer.Header().Set("Access-Control-Allow-Methods", "*")
	c.Writer.WriteHeader(200)
	c.Writer.Flush()

	buf := make([]byte, 65536)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				s.Close()
				return
			}
			c.Writer.Flush()
		}
		if err != nil {
			return
		}
	}
}

// handleSegment403 redirects to the main room URL when a segment request
// returns 403. This causes the player to re-fetch the playlist through the
// initial request path which has full pipe-based retry support.
func handleSegment403(c *gin.Context, platform, room, format string) {
	redirectURL := fmt.Sprintf("/%s/%s", platform, room)
	q := url.Values{}
	if format != "" {
		q.Set("format", format)
	}
	if p := c.DefaultQuery("proxy", ""); p != "" {
		q.Set("proxy", p)
	}
	if len(q) > 0 {
		redirectURL += "?" + q.Encode()
	}
	c.Redirect(302, redirectURL)
}

// formatFromURL determines the stream format from a URL: "ws", "flv", "m3u8", etc.
func formatFromURL(u *url.URL) string {
	switch u.Scheme {
	case "ws", "wss":
		return "ws"
	default:
		return strings.TrimPrefix(path.Ext(u.Path), ".")
	}
}

func Forwarder(c *gin.Context) {
	log := global.Log.WithField("function", "app.http.controllers.Forwarder")
	log.WithField("http request", "headers").Debug(c.Request.Header)
	proxy := c.GetString("proxy")
	format := c.DefaultQuery("format", "")
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

	pp := c.DefaultQuery("url", "")
	prefix := buildPrefix(c)

	switch strings.ToLower(c.Param("platform")) {
	case "douyu":
		if pp != "" {
			// Segment request
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					handleSegment403(c, "douyu", c.Param("room"), format)
					return
				}
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
			return
		}

		// initialFormat tracks the format from the first successful extraction,
		// so retries can ensure the same format is returned.
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			dy, err := DouYu.NewDouyuLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			u, err := dy.GetLink()
			if err != nil {
				return nil, fmt.Errorf("get link error: %w", err)
			}
			result := &stream.ExtractResult{URL: u.String()}
			if previous != nil {
				// Validate format consistency on retry
				newFmt := formatFromURL(u)
				if newFmt != initialFormat {
					return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, newFmt)
				}
			} else {
				initialFormat = formatFromURL(u)
			}
			return result, nil
		}

		result, err := extractFn(nil)
		if err != nil {
			log.Errorf("initial extract error: %s\n", err.Error())
			c.String(400, err.Error())
			return
		}
		u, _ := url.Parse(result.URL)
		switch u.Scheme {
		case "http", "https":
			f := httpweb.NewHTTPWebForwarder(proxyURL, false)
			s := f.Stream(extractFn)
			streamToClient(c, s, "video/x-flv")
		case "ws", "wss":
			f := websocket.NewWebSocketForwarderWithRetry(proxyURL, false, extractFn)
			err = f.Start(c, result.URL)
			if err != nil {
				log.Errorf("forward ws(s) stream error: %s\n", err.Error())
				return
			}
		default:
			c.String(500, "unsupported schema")
			return
		}
		return

	case "huya":
		if pp != "" {
			p := hls.NewHLSForwarder(proxyURL, true)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					handleSegment403(c, "huya", c.Param("room"), format)
					return
				}
				log.WithField("url", pp).Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
			return
		}

		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := HuYa.NewHuyaLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			u, err := link.GetLink(format)
			if err != nil {
				return nil, fmt.Errorf("get link error: %w", err)
			}
			result := &stream.ExtractResult{URL: u.String()}
			if previous != nil {
				newFmt := formatFromURL(u)
				if newFmt != initialFormat {
					return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, newFmt)
				}
			} else {
				initialFormat = formatFromURL(u)
			}
			return result, nil
		}

		result, err := extractFn(nil)
		if err != nil {
			log.Errorf("initial extract error: %s\n", err.Error())
			c.String(500, err.Error())
			return
		}
		u, _ := url.Parse(result.URL)
		switch path.Ext(u.Path) {
		case ".m3u8":
			p := hls.NewHLSForwarder(proxyURL, true)
			err := p.WrapPlaylist(c, result.URL, prefix)
			if err != nil {
				log.Errorf("wrap play list error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		case ".flv":
			f := httpweb.NewHTTPWebForwarder(proxyURL, true)
			s := f.Stream(extractFn)
			streamToClient(c, s, "video/x-flv")
		default:
			c.String(500, "unsupported format")
			return
		}
		return

	case "twitch":
		if pp != "" {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					handleSegment403(c, "twitch", c.Param("room"), format)
					return
				}
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
			return
		}

		link, err := Twitch.NewTwitchLink(c.Param("room"), proxyURL)
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
		p := hls.NewHLSForwarder(proxyURL, false)
		err = p.ForwardM3u8(c, u.String(), prefix)
		if err != nil {
			log.Errorf("forward playlist error: %s\n", err.Error())
			c.String(400, err.Error())
			return
		}
		return

	case "bilibili":
		if pp != "" {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					handleSegment403(c, "bilibili", c.Param("room"), format)
					return
				}
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
			return
		}

		headers := make(http.Header)
		headers.Set("Referer", "https://live.bilibili.com")
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := BiliBili.NewBiliBiliLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			u, err := link.GetLink()
			if err != nil {
				return nil, fmt.Errorf("get link error: %w", err)
			}
			result := &stream.ExtractResult{URL: u.String(), Headers: headers}
			if previous != nil {
				newFmt := formatFromURL(u)
				if newFmt != initialFormat {
					return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, newFmt)
				}
			} else {
				initialFormat = formatFromURL(u)
			}
			return result, nil
		}

		result, err := extractFn(nil)
		if err != nil {
			log.Errorf("initial extract error: %s\n", err.Error())
			c.String(500, err.Error())
			return
		}
		u, _ := url.Parse(result.URL)
		switch path.Ext(u.Path) {
		case ".m3u8":
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.WrapPlaylist(c, result.URL, prefix)
			if err != nil {
				log.Errorf("wrap play list error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		case ".flv":
			f := httpweb.NewHTTPWebForwarder(proxyURL, false)
			s := f.Stream(extractFn)
			streamToClient(c, s, "video/x-flv")
		default:
			c.String(500, "unsupported format")
			return
		}
		return

	case "douyin":
		if pp != "" {
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.Forward(c, pp, prefix)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					handleSegment403(c, "douyin", c.Param("room"), format)
					return
				}
				log.Errorf("forward hls stream error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
			return
		}

		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := DouYin.NewDouYinLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			u, err := link.GetLink(format)
			if err != nil {
				return nil, fmt.Errorf("get link error: %w", err)
			}
			result := &stream.ExtractResult{URL: u.String()}
			if previous != nil {
				newFmt := formatFromURL(u)
				if newFmt != initialFormat {
					return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, newFmt)
				}
			} else {
				initialFormat = formatFromURL(u)
			}
			return result, nil
		}

		result, err := extractFn(nil)
		if err != nil {
			log.Errorf("initial extract error: %s\n", err.Error())
			c.String(500, err.Error())
			return
		}
		u, _ := url.Parse(result.URL)
		switch path.Ext(u.Path) {
		case ".m3u8":
			p := hls.NewHLSForwarder(proxyURL, false)
			err := p.WrapPlaylist(c, result.URL, prefix)
			if err != nil {
				log.Errorf("wrap play list error: %s\n", err.Error())
				c.String(400, err.Error())
				return
			}
		case ".flv":
			f := httpweb.NewHTTPWebForwarder(proxyURL, false)
			s := f.Stream(extractFn)
			streamToClient(c, s, "video/x-flv")
		default:
			c.String(500, "unsupported format")
			return
		}
		return

	default:
		c.String(400, "unsupported platform")
		return
	}
}
