package controllers

import (
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"

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

// formatFromURL determines the stream format from a URL: "ws", "flv",
// "m3u8", etc. DouYu's p2p=2 ".xs" URLs serve the same http-flv payload as
// ".flv", so they normalize to "flv" — dispatchStream already routes both to
// the FLV forwarder, and treating them as one format lets a mid-stream
// switch continue seamlessly instead of tripping the consistency check.
func formatFromURL(u *url.URL) string {
	switch u.Scheme {
	case "ws", "wss":
		return "ws"
	}
	if ext := path.Ext(u.Path); ext == ".xs" {
		return "flv"
	} else if ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return ""
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

	// 2b. Inject cookie into the extractor if supported.
	// Cookie is set in context by middleware: from ?cookie= (decoded) or --bilibili-cookie (raw).
	if cs, ok := ext.(extractor.CookieSetter); ok {
		if rawCookie := c.GetString("bilibili-cookie"); rawCookie != "" && platform == "bilibili" {
			cs.SetCookie(rawCookie)
		}
	}

	// 3. Resolve the desired format.
	desiredFormat := resolveDesiredFormat(format, ext)

	// 4. Build the unified extractFn closure. The first extraction (previous
	// ==nil) is cached inside buildExtractFn so that dispatch routing and the
	// chosen forwarder reuse it instead of hitting the upstream extractor
	// twice (DouYu/Twitch would otherwise be called once for routing and
	// again by the forwarder's produce loop).
	extractFn := buildExtractFn(ext, desiredFormat)

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

// formatDeadlockThreshold is how many consecutive format mismatches are
// tolerated before the retry is reported as a terminal stream.ErrFormatDeadlock.
// A couple of retries absorb transient platform blips; beyond that the
// platform (e.g. DouYu moving a room to p2p) has switched for good, and the
// client must reconnect to land in the forwarder matching the new format.
const formatDeadlockThreshold = 3

// buildExtractFn wraps an extractor in an ExtractFunc that enforces format
// consistency on retry and caches the first extraction.
//
// The first call (previous==nil) extracts with desiredFormat, caches the
// result, and records the resolved initial format; subsequent nil calls
// return the cached result without re-invoking the extractor. This lets the
// dispatch routing decision (which needs the URL to pick a forwarder) and the
// chosen forwarder's produce loop share a single upstream call instead of each
// extracting separately (which would double DouYu/Twitch API hits).
//
// On retry (previous!=nil) it re-extracts fresh using the initial format and
// rejects URLs whose format differs, so a stream never switches between
// FLV/HLS/WebSocket mid-flight. When the mismatch persists for
// formatDeadlockThreshold consecutive retries, the error wraps
// stream.ErrFormatDeadlock: producers then stop looping and close, and the
// client's reconnect re-dispatches into the forwarder the platform now
// serves (e.g. websocket for p2p rooms).
func buildExtractFn(ext extractor.Extractor, desiredFormat string) stream.ExtractFunc {
	var (
		initialFormat  string
		initOnce       sync.Once
		initResult     *stream.ExtractResult
		initErr        error
		formatMismatch int
	)
	// doExtract performs a fresh extraction and returns the wrapped result
	// together with the format derived from the URL.
	doExtract := func(format string) (*stream.ExtractResult, string, error) {
		result, err := ext.Extract(format)
		if err != nil {
			return nil, "", fmt.Errorf("extract error: %w", err)
		}
		streamResult := &stream.ExtractResult{
			URL:             result.URL,
			Headers:         result.Headers,
			ExpireAt:        result.ExpireAt,
			VariantSelector: result.VariantSelector,
		}
		u, parseErr := url.Parse(result.URL)
		if parseErr != nil {
			return nil, "", fmt.Errorf("parse extracted URL error: %w", parseErr)
		}
		return streamResult, formatFromURL(u), nil
	}
	return func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
		if previous == nil {
			// First extraction: cache so dispatch + forwarder reuse one call.
			initOnce.Do(func() {
				r, fmtName, err := doExtract(desiredFormat)
				if err != nil {
					initErr = err
					return
				}
				initResult = r
				initialFormat = fmtName
			})
			if initErr != nil {
				return nil, initErr
			}
			return initResult, nil
		}
		// Retry: re-extract fresh using the initial format and validate.
		r, fmtName, err := doExtract(initialFormat)
		if err != nil {
			return nil, err
		}
		if fmtName != initialFormat {
			formatMismatch++
			if formatMismatch > formatDeadlockThreshold {
				log := global.Log.WithField("func", "app.http.controllers.buildExtractFn")
				log.Errorf("stream format changed from %s to %s permanently after %d retries", initialFormat, fmtName, formatDeadlockThreshold)
				return nil, fmt.Errorf("stream format changed from %s to %s permanently: %w", initialFormat, fmtName, stream.ErrFormatDeadlock)
			}
			return nil, fmt.Errorf("format changed from %s to %s, will retry", initialFormat, fmtName)
		}
		formatMismatch = 0
		return r, nil
	}
}
