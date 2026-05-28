package hls

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	libm3u8 "github.com/grafov/m3u8"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// HLSStream continuously fetches an HLS playlist, downloads segments in order,
// and pipes raw MPEG-TS data to the client via a Pipe. Each client gets its
// own HLSStream with independent upstream connections.
type HLSStream struct {
	pipe      *stream.Pipe
	done      chan struct{}
	closeErr  error
	closeOnce sync.Once
	hc        *http.Client
	extractFn stream.ExtractFunc

	// refreshCh signals the produce loop to re-extract before the URL expires.
	refreshCh chan struct{}
}

func NewHLSStream(extractFn stream.ExtractFunc, hc *http.Client) *HLSStream {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.NewHLSStream")
	log.Debug("creating HLSStream")
	s := &HLSStream{
		pipe:      stream.NewPipe(),
		done:      make(chan struct{}),
		hc:        hc,
		extractFn: extractFn,
		refreshCh: make(chan struct{}, 1),
	}
	go s.produce()
	return s
}

func (s *HLSStream) Read(p []byte) (int, error) {
	return s.pipe.Read(p)
}

func (s *HLSStream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.HLSStream.Close")
	log.Debug("closing HLSStream")
	s.closeOnce.Do(func() {
		s.pipe.BreakWithError(io.ErrClosedPipe)
		close(s.done)
	})
	return nil
}

func (s *HLSStream) Wait() error {
	<-s.done
	return s.closeErr
}

func (s *HLSStream) closeWithError(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.HLSStream.closeWithError")
	log.Warnf("closing HLSStream with error: %s", err.Error())
	s.closeErr = err
	s.pipe.CloseWithError(err)
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

func (s *HLSStream) produce() {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.HLSStream.produce")

	var previous *stream.ExtractResult
	var mediaPlaylistURL string
	var currentHeaders http.Header
	var lastSeqID uint64
	var hasLastSeqID bool
	var initSegmentFetched bool

	// scheduleRefresh sets a timer to trigger re-extraction before the URL expires.
	var refreshTimer *time.Timer
	scheduleRefresh := func(expireAt *time.Time) {
		if refreshTimer != nil {
			refreshTimer.Stop()
			refreshTimer = nil
		}
		if expireAt == nil {
			return
		}
		// Refresh 60 seconds before expiry, but no less than 5 seconds from now.
		leadTime := 60 * time.Second
		remaining := time.Until(*expireAt)
		if remaining <= leadTime+5*time.Second {
			leadTime = remaining - 5*time.Second
			if leadTime < 0 {
				leadTime = 0
			}
		}
		when := remaining - leadTime
		if when < 0 {
			when = 0
		}
		log.Debugf("scheduling token refresh in %s (expires at %s)", when, expireAt.Format(time.RFC3339))
		refreshTimer = time.AfterFunc(when, func() {
			select {
			case s.refreshCh <- struct{}{}:
			default:
			}
		})
	}
	defer func() {
		if refreshTimer != nil {
			refreshTimer.Stop()
		}
	}()

	for {
		// Check if client disconnected.
		if s.pipe.Err() != nil {
			return
		}

		// Extract phase: get the initial m3u8 URL.
		if mediaPlaylistURL == "" {
			result, err := s.extractFn(previous)
			if err != nil {
				log.Warnf("extract error: %s", err.Error())
				time.Sleep(2 * time.Second)
				continue
			}
			previous = result
			mediaPlaylistURL = result.URL
			currentHeaders = result.Headers
			hasLastSeqID = false
			initSegmentFetched = false
			scheduleRefresh(result.ExpireAt)
			continue
		}

		// Poll phase: fetch and parse the playlist.
		playlist, listType, err := fetchAndParseM3U8(s.hc, mediaPlaylistURL, currentHeaders)
		if err != nil {
			if isExpiredHLS(err) {
				log.Warnf("playlist fetch 403, re-extracting: %s", err.Error())
				mediaPlaylistURL = ""
				continue
			}
			if isTransientHLS(err) {
				log.Warnf("playlist fetch transient error, retrying: %s", err.Error())
				time.Sleep(2 * time.Second)
				continue
			}
			log.Errorf("playlist fetch error: %s", err.Error())
			s.closeWithError(err)
			return
		}

		switch listType {
		case libm3u8.MASTER:
			masterpl := playlist.(*libm3u8.MasterPlaylist)
			if len(masterpl.Variants) == 0 {
				log.Warnln("master playlist has no variants, re-extracting")
				mediaPlaylistURL = ""
				continue
			}
			variant := pickHighestBandwidthVariant(masterpl.Variants)
			resolved := resolveURL(mediaPlaylistURL, variant.URI)
			mediaPlaylistURL = resolved
			continue // Re-fetch as media playlist.

		case libm3u8.MEDIA:
			mediapl := playlist.(*libm3u8.MediaPlaylist)

			// Fetch init segment (#EXT-X-MAP) if present.
			if mediapl.Map != nil && mediapl.Map.URI != "" && !initSegmentFetched {
				segURL := resolveURL(mediaPlaylistURL, mediapl.Map.URI)
				if err := s.fetchAndPipeSegment(segURL, currentHeaders); err != nil {
					if isExpiredHLS(err) {
						log.Warnf("init segment fetch 403, re-extracting: %s", err.Error())
						mediaPlaylistURL = ""
						continue
					}
					if isTransientHLS(err) {
						log.Warnf("init segment fetch transient error, retrying: %s", err.Error())
						time.Sleep(2 * time.Second)
						continue
					}
					s.closeWithError(err)
					return
				}
				initSegmentFetched = true
			}

			// Download new segments in order.
			for _, seg := range mediapl.Segments {
				if seg == nil {
					continue
				}
				if hasLastSeqID && seg.SeqId <= lastSeqID {
					continue
				}

				segURL := resolveURL(mediaPlaylistURL, seg.URI)
				if err := s.fetchAndPipeSegment(segURL, currentHeaders); err != nil {
					if isExpiredHLS(err) {
						log.Warnf("segment fetch 403, re-extracting: %s", err.Error())
						mediaPlaylistURL = ""
						break
					}
					if isTransientHLS(err) {
						log.Warnf("segment fetch transient error, skipping: %s", err.Error())
						time.Sleep(1 * time.Second)
						continue
					}
					s.closeWithError(err)
					return
				}

				lastSeqID = seg.SeqId
				hasLastSeqID = true

				if s.pipe.Err() != nil {
					return
				}
			}

			// VOD ended.
			if mediapl.Closed {
				s.closeWithError(io.EOF)
				return
			}

		default:
			log.Warnf("unknown playlist type: %d, re-extracting", listType)
			mediaPlaylistURL = ""
			continue
		}

		if mediaPlaylistURL == "" {
			// Reset happened during segment loop; re-extract.
			continue
		}

		// Sleep before next poll, but wake early if token needs refreshing.
		targetDur := 3 * time.Second
		if ml, ok := playlist.(*libm3u8.MediaPlaylist); ok && ml.TargetDuration > 0 {
			targetDur = time.Duration(ml.TargetDuration) * time.Second
		}
		if targetDur < time.Second {
			targetDur = time.Second
		}

		select {
		case <-s.refreshCh:
			log.Infoln("token refresh triggered, re-extracting")
			mediaPlaylistURL = ""
		case <-time.After(targetDur):
		}

		if s.pipe.Err() != nil {
			return
		}
	}
}

func (s *HLSStream) fetchAndPipeSegment(segURL string, headers http.Header) error {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.HLSStream.fetchAndPipeSegment")
	resp, err := doRequestWithHeaders(s.hc, "GET", segURL, headers)
	if err != nil {
		log.Warnf("fetch segment error: %s", err.Error())
		return fmt.Errorf("fetch segment error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warnf("fetch segment got status: %s", resp.Status)
		return fmt.Errorf("fetch segment err got: %s", resp.Status)
	}
	_, err = io.Copy(s.pipe, resp.Body)
	if err != nil {
		log.Warnf("pipe segment data error: %s", err.Error())
	}
	return err
}

func fetchAndParseM3U8(hc *http.Client, m3u8URL string, headers http.Header) (libm3u8.Playlist, libm3u8.ListType, error) {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.fetchAndParseM3U8")
	resp, err := doRequestWithHeaders(hc, "GET", m3u8URL, headers)
	if err != nil {
		log.Warnf("get m3u8 file error: %s", err.Error())
		return nil, 0, fmt.Errorf("get m3u8 file error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warnf("get m3u8 got status: %s", resp.Status)
		return nil, 0, fmt.Errorf("get m3u8 err got: %s", resp.Status)
	}
	return libm3u8.DecodeFrom(resp.Body, true)
}

func doRequestWithHeaders(hc *http.Client, method, rawURL string, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k := range headers {
		req.Header.Set(k, headers.Get(k))
	}
	return hc.Do(req)
}

func resolveURL(baseURL, refURL string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return refURL
	}
	ref, err := url.Parse(refURL)
	if err != nil {
		return refURL
	}
	return base.ResolveReference(ref).String()
}

// isExpiredHLS reports whether the error indicates the URL has expired (403),
// requiring re-extraction to obtain a fresh URL.
func isExpiredHLS(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "403")
}

// isTransientHLS reports whether the error is a transient network issue
// (EOF, connection reset, timeout) that can be retried with the same URL.
func isTransientHLS(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "EOF") {
		return true
	}
	if strings.Contains(msg, "connection reset") {
		return true
	}
	if strings.Contains(msg, "temporary") {
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	return false
}

func pickHighestBandwidthVariant(variants []*libm3u8.Variant) *libm3u8.Variant {
	log := global.Log.WithField("func", "app.engine.forwarder.hls.pickHighestBandwidthVariant")
	best := variants[0]
	for _, v := range variants[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	log.Debugf("selected variant bandwidth=%d uri=%s", best.Bandwidth, best.URI)
	return best
}
