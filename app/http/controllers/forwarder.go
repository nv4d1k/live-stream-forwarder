package controllers

import (
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	// Trigger platform registration via init().
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/BiliBili"
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYin"
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/DouYu"
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/HuYa"
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/Kick"
	_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/Twitch"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
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
// parameter and the extractor's declared capabilities. If no format is
// specified, it picks based on the supported formats: single-format extractors
// use their only format; multi-format extractors with both flv and m3u8
// randomly pick between them (preserving original behavior); otherwise it
// falls back to the extractor's default.
func resolveDesiredFormat(queryFormat string, ext extractor.Extractor) string {
	if queryFormat != "" {
		return queryFormat
	}
	formats := ext.SupportedFormats()
	if len(formats) == 1 {
		return formats[0]
	}
	hasFLV := slices.Contains(formats, "flv")
	hasM3U8 := slices.Contains(formats, "m3u8")
	if hasFLV && hasM3U8 {
		if rand.Intn(2) == 0 {
			return "flv"
		}
		return "m3u8"
	}
	return ext.DefaultFormat()
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

// dispatchStream routes the stream to the appropriate forwarder based on URL
// scheme and path extension.
func dispatchStream(c *gin.Context, u *url.URL, extractFn stream.ExtractFunc, proxyURL *url.URL, mobile bool, key string) {
	switch u.Scheme {
	case "ws", "wss":
		f := websocket.NewWebSocketForwarderWithRetry(proxyURL, mobile, extractFn, key)
		err := f.Start(c, u.String())
		if err != nil {
			global.Log.WithField("func", "app.http.controllers.dispatchStream").
				Errorf("forward ws(s) stream error: %s\n", err.Error())
		}
	default:
		switch path.Ext(u.Path) {
		case ".m3u8":
			h := hls.NewHLSForwarder(proxyURL, mobile)
			s := h.Stream(extractFn)
			streamToClient(c, s, "video/mp2t")
		case ".flv", ".xs":
			streamToClient(c, flvStreamWithCache(extractFn, proxyURL, mobile, key), "video/x-flv")
		default:
			c.String(500, "unsupported format")
		}
	}
}

func Forwarder(c *gin.Context) {
	log := global.Log.WithField("func", "app.http.controllers.Forwarder")
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

	platform := strings.ToLower(c.Param("platform"))
	room := c.Param("room")

	log.WithField("field", "url path").Debug(c.Request.URL.Path)
	log.WithField("field", "room").Debugf("%s %s\n", platform, room)
	log.WithField("field", "query").Debug(c.Request.URL.RawQuery)
	if proxyURL != nil {
		log.WithField("field", "proxy").Debug(proxyURL.String())
	}
	log = log.WithField("platform", platform).WithField("room", room)

	// 1. Look up the platform in the registry.
	entry, ok := extractor.Registry[platform]
	if !ok {
		c.String(400, "unsupported platform")
		return
	}

	// 2. Create the extractor instance.
	ext, err := entry.Factory(room, proxyURL)
	if err != nil {
		log.Errorf("create extractor error: %s\n", err.Error())
		c.String(entry.InitialError, err.Error())
		return
	}

	// 3. Resolve the desired format.
	desiredFormat := resolveDesiredFormat(format, ext)

	// 4. Build the unified extractFn closure.
	var initialFormat string
	extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
		extractFormat := desiredFormat
		if previous != nil {
			extractFormat = initialFormat
		}
		result, err := ext.Extract(extractFormat)
		if err != nil {
			return nil, fmt.Errorf("extract error: %w", err)
		}
		streamResult := &stream.ExtractResult{
			URL:      result.URL,
			Headers:  result.Headers,
			ExpireAt: result.ExpireAt,
		}
		u, parseErr := url.Parse(result.URL)
		if parseErr != nil {
			return nil, fmt.Errorf("parse extracted URL error: %w", parseErr)
		}
		if previous != nil {
			newFmt := formatFromURL(u)
			if newFmt != initialFormat {
				return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, newFmt)
			}
		} else {
			initialFormat = formatFromURL(u)
		}
		return streamResult, nil
	}

	// 5. Perform initial extraction.
	result, err := extractFn(nil)
	if err != nil {
		log.Errorf("initial extract error: %s\n", err.Error())
		c.String(entry.InitialError, err.Error())
		return
	}

	// 6. Dispatch to the appropriate forwarder.
	u, _ := url.Parse(result.URL)
	key := fmt.Sprintf("%s:%s", platform, room)
	dispatchStream(c, u, extractFn, proxyURL, entry.Mobile, key)
}
