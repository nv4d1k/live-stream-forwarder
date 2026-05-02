package controllers

import (
	"fmt"
	"io"
	"math/rand"
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
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/flv"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/hls"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/websocket"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// streamToClient writes data from an io.ReadCloser to the gin context as a
// long-lived HTTP stream. The client sees a single continuous response
// even if the producer reconnects on 403.
func streamToClient(c *gin.Context, r io.ReadCloser, contentType string) {
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
		n, err := r.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				r.Close()
				return
			}
			c.Writer.Flush()
		}
		if err != nil {
			r.Close()
			return
		}
	}
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

// resolveDesiredFormat determines the desired stream format from the query
// parameter. If no format is specified, it randomly selects "flv" or "m3u8".
func resolveDesiredFormat(queryFormat string) string {
	if queryFormat != "" {
		return queryFormat
	}
	if rand.Intn(2) == 0 {
		return "flv"
	}
	return "m3u8"
}

// flvStreamWithCache creates an FLV stream with header caching support.
func flvStreamWithCache(extractFn stream.ExtractFunc, proxyURL *url.URL, mobile bool, key string) io.ReadCloser {
	f := httpweb.NewHTTPWebForwarder(proxyURL, mobile)
	writerWrapper := func(w io.Writer) io.Writer {
		return flv.NewHeaderCacheWriter(w, flv.DefaultCache, key)
	}
	s := f.Stream(extractFn, stream.WithWriterWrapper(writerWrapper))
	return flv.NewFLVStream(s, flv.DefaultCache, key)
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

	switch strings.ToLower(c.Param("platform")) {
	case "douyu":
		desiredFormat := resolveDesiredFormat(format)
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			dy, err := DouYu.NewDouyuLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			// On retry, pass the known format so the extractor targets it.
			extractFormat := desiredFormat
			if previous != nil {
				extractFormat = initialFormat
			}
			u, err := dy.GetLink(extractFormat)
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
			c.String(400, err.Error())
			return
		}
		u, _ := url.Parse(result.URL)
		key := fmt.Sprintf("douyu:%s", c.Param("room"))
		switch u.Scheme {
		case "http", "https":
			streamToClient(c, flvStreamWithCache(extractFn, proxyURL, false, key), "video/x-flv")
		case "ws", "wss":
			f := websocket.NewWebSocketForwarderWithRetry(proxyURL, false, extractFn, key)
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
		desiredFormat := resolveDesiredFormat(format)
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := HuYa.NewHuyaLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			// On retry, use the known format to avoid format switching.
			extractFormat := desiredFormat
			if previous != nil {
				extractFormat = initialFormat
			}
			u, err := link.GetLink(extractFormat)
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
		key := fmt.Sprintf("huya:%s", c.Param("room"))
		switch path.Ext(u.Path) {
		case ".m3u8":
			h := hls.NewHLSForwarder(proxyURL, true)
			s := h.Stream(extractFn)
			streamToClient(c, s, "video/mp2t")
		case ".flv":
			streamToClient(c, flvStreamWithCache(extractFn, proxyURL, true, key), "video/x-flv")
		default:
			c.String(500, "unsupported format")
			return
		}
		return

	case "twitch":
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := Twitch.NewTwitchLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			u, err := link.GetLink("m3u8")
			if err != nil {
				return nil, fmt.Errorf("get link error: %w", err)
			}
			return &stream.ExtractResult{URL: u.String()}, nil
		}

		h := hls.NewHLSForwarder(proxyURL, false)
		s := h.Stream(extractFn)
		streamToClient(c, s, "video/mp2t")
		return

	case "bilibili":
		desiredFormat := resolveDesiredFormat(format)
		headers := make(http.Header)
		headers.Set("Referer", "https://live.bilibili.com")
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := BiliBili.NewBiliBiliLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			// On retry, use the known format to avoid format switching.
			extractFormat := desiredFormat
			if previous != nil {
				extractFormat = initialFormat
			}
			u, err := link.GetLink(extractFormat)
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
		key := fmt.Sprintf("bilibili:%s", c.Param("room"))
		switch path.Ext(u.Path) {
		case ".m3u8":
			h := hls.NewHLSForwarder(proxyURL, false)
			s := h.Stream(extractFn)
			streamToClient(c, s, "video/mp2t")
		case ".flv":
			streamToClient(c, flvStreamWithCache(extractFn, proxyURL, false, key), "video/x-flv")
		default:
			c.String(500, "unsupported format")
			return
		}
		return

	case "douyin":
		desiredFormat := resolveDesiredFormat(format)
		var initialFormat string
		extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
			link, err := DouYin.NewDouYinLink(c.Param("room"), proxyURL)
			if err != nil {
				return nil, fmt.Errorf("create link object error: %w", err)
			}
			// On retry, use the known format to avoid format switching.
			extractFormat := desiredFormat
			if previous != nil {
				extractFormat = initialFormat
			}
			u, err := link.GetLink(extractFormat)
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
		key := fmt.Sprintf("douyin:%s", c.Param("room"))
		switch path.Ext(u.Path) {
		case ".m3u8":
			h := hls.NewHLSForwarder(proxyURL, false)
			s := h.Stream(extractFn)
			streamToClient(c, s, "video/mp2t")
		case ".flv":
			streamToClient(c, flvStreamWithCache(extractFn, proxyURL, false, key), "video/x-flv")
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
