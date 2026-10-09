package YouTube

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	libm3u8 "github.com/grafov/m3u8"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.YouTube.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("youtube", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewYouTubeLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

// Link implements extractor.Extractor and extractor.QualitySetter for
// YouTube live streams.
type Link struct {
	rid     string
	quality string
	client  *http.Client
	apiBase string
}

// videoIDPattern matches the 11-character YouTube video IDs
// (e.g. WWTcu33u00A).
var videoIDPattern = regexp.MustCompile(`^[\w-]{11}$`)

// NewYouTubeLink creates a YouTube extractor for the given video ID.
func NewYouTubeLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.YouTube.NewYouTubeLink")
	if !videoIDPattern.MatchString(rid) {
		log.Warnf("invalid YouTube video ID %q", rid)
		return nil, fmt.Errorf("invalid YouTube video ID %q: expected an 11-character ID", rid)
	}
	y := &Link{rid: rid, apiBase: defaultAPIBase}
	if proxy != nil {
		y.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	} else {
		y.client = &http.Client{Transport: &http.Transport{}}
	}
	log.Debugf("creating YouTube extractor for room %s", rid)
	log.Infof("YouTube extractor created for room %s", rid)
	return y, nil
}

// SetQuality implements extractor.QualitySetter. The hint (e.g. "720p")
// selects the HLS variant closest to the requested height without
// exceeding it.
func (l *Link) SetQuality(quality string) {
	log := global.Log.WithField("func", "app.engine.extractor.YouTube.SetQuality")
	log.Debugf("setting quality %q for room %s", quality, l.rid)
	l.quality = quality
}

// Extract resolves the live manifest URL via the innertube player API.
// format selects the manifest family: "dash" returns the DASH MPD (fMP4
// representations up to and beyond 1080p), anything else returns the HLS
// master playlist (muxed TS variants up to 1080p). The returned ExpireAt
// comes from the manifest URL's /expire/<unix>/ path segment (about 6 hours
// out), letting the forwarders re-extract before the URL goes stale.
func (l *Link) Extract(format string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.YouTube.Extract")

	pr, err := l.getPlayer()
	if err != nil {
		log.Errorf("failed to get player response for room %s: %v", l.rid, err)
		return nil, err
	}

	if pr.PlayabilityStatus.Status != "OK" {
		log.Warnf("room %s not playable: %s %s", l.rid, pr.PlayabilityStatus.Status, pr.PlayabilityStatus.Reason)
		return nil, fmt.Errorf("room %s not playable: %s %s", l.rid, pr.PlayabilityStatus.Status, pr.PlayabilityStatus.Reason)
	}
	if !pr.VideoDetails.IsLive {
		log.Warnf("room %s is not live", l.rid)
		return nil, fmt.Errorf("room %s is not live", l.rid)
	}

	var manifestURL string
	if format == "dash" {
		if pr.StreamingData.DASHManifestURL == "" {
			log.Warnf("room %s has no DASH manifest URL", l.rid)
			return nil, fmt.Errorf("room %s has no DASH manifest URL", l.rid)
		}
		manifestURL = pr.StreamingData.DASHManifestURL
	} else {
		if pr.StreamingData.HLSManifestURL == "" {
			log.Warnf("room %s has no HLS manifest URL", l.rid)
			return nil, fmt.Errorf("room %s has no HLS manifest URL", l.rid)
		}
		manifestURL = pr.StreamingData.HLSManifestURL
	}

	result := &extractor.Result{URL: manifestURL}
	if exp, err := expireAtFromManifestURL(manifestURL); err == nil {
		result.ExpireAt = exp
		log.Debugf("manifest URL expires at %s for room %s", exp.Format(time.RFC3339), l.rid)
	} else {
		log.Debugf("could not parse expire from manifest URL for room %s: %v", l.rid, err)
	}

	if h := parseQualityHeight(l.quality); h > 0 {
		result.VariantSelector = variantSelectorForHeight(h)
		log.Debugf("variant selector targets %dp for room %s", h, l.rid)
	} else if l.quality != "" {
		log.Warnf("invalid quality %q for room %s, falling back to highest bandwidth", l.quality, l.rid)
	}

	log.Debugf("extracted stream URL for room %s", l.rid)
	return result, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"m3u8", "dash"}
}

func (l *Link) DefaultFormat() string {
	return "m3u8"
}

// expireAtFromManifestURL extracts the expiry timestamp from a googlevideo
// manifest URL path segment (/expire/<unix>/). Returns an error when the
// segment is missing or malformed, in which case the caller falls back to
// error-driven refresh.
func expireAtFromManifestURL(manifestURL string) (*time.Time, error) {
	u, err := url.Parse(manifestURL)
	if err != nil {
		return nil, fmt.Errorf("parse manifest URL: %w", err)
	}
	segs := strings.Split(u.Path, "/")
	for i, seg := range segs {
		if seg == "expire" && i+1 < len(segs) {
			ts, err := strconv.ParseInt(segs[i+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse expire timestamp %q: %w", segs[i+1], err)
			}
			t := time.Unix(ts, 0)
			return &t, nil
		}
	}
	return nil, fmt.Errorf("no expire path segment in manifest URL")
}

// parseQualityHeight converts a quality hint like "720p" or "720" into a
// pixel height. Returns 0 when the hint cannot be parsed.
func parseQualityHeight(quality string) int {
	q := strings.TrimSuffix(strings.TrimSpace(strings.ToLower(quality)), "p")
	h, err := strconv.Atoi(q)
	if err != nil || h <= 0 {
		return 0
	}
	return h
}

// variantHeight parses the height out of an HLS variant RESOLUTION value
// like "1920x1080". Returns 0 when it cannot be parsed.
func variantHeight(resolution string) int {
	parts := strings.SplitN(resolution, "x", 2)
	if len(parts) != 2 {
		return 0
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0
	}
	return h
}

// variantSelectorForHeight builds a VariantSelector that picks the tallest
// variant not exceeding the target height; when every variant is taller it
// picks the shortest one. Variants whose RESOLUTION cannot be parsed are
// ignored, and when none parse the selector falls back to the highest
// bandwidth. It never returns nil for a non-empty variant list.
func variantSelectorForHeight(target int) func([]*libm3u8.Variant) *libm3u8.Variant {
	return func(variants []*libm3u8.Variant) *libm3u8.Variant {
		log := global.Log.WithField("func", "app.engine.extractor.YouTube.variantSelectorForHeight")

		var best *libm3u8.Variant
		bestH := 0
		for _, v := range variants {
			h := variantHeight(v.Resolution)
			if h <= 0 || h > target {
				continue
			}
			if best == nil || h > bestH {
				best, bestH = v, h
			}
		}
		if best != nil {
			log.Debugf("selected variant height=%d uri=%s for target %dp", bestH, best.URI, target)
			return best
		}

		// Every variant exceeds the target: take the shortest available.
		var shortest *libm3u8.Variant
		shortestH := 0
		for _, v := range variants {
			h := variantHeight(v.Resolution)
			if h <= 0 {
				continue
			}
			if shortest == nil || h < shortestH {
				shortest, shortestH = v, h
			}
		}
		if shortest != nil {
			log.Debugf("all variants exceed target %dp, selected shortest height=%d uri=%s", target, shortestH, shortest.URI)
			return shortest
		}

		// No parsable resolutions: fall back to highest bandwidth.
		fallback := variants[0]
		for _, v := range variants[1:] {
			if v.Bandwidth > fallback.Bandwidth {
				fallback = v
			}
		}
		log.Debugf("no parsable resolutions for target %dp, selected highest bandwidth=%d uri=%s", target, fallback.Bandwidth, fallback.URI)
		return fallback
	}
}
