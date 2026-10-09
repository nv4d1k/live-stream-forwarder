package dash

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// pollInterval is the pause between track batch fetches (YouTube's MPD
// minimumUpdatePeriod is 2s). Package-level so tests can shorten it.
var pollInterval = 2 * time.Second

// startBackoff is how many segments behind the live edge a stream starts,
// trading a few seconds of latency for a stable starting point.
const startBackoff = 3

// notFoundLimit is how many consecutive 404 batches (segment not generated
// yet) are tolerated before re-extracting, catching streams that ended.
const notFoundLimit = 30

// DASHStream continuously fetches the audio and video representations of a
// YouTube live DASH manifest and pipes an interleaved two-track fMP4 stream
// to the client: one merged init (ftyp + moov with both traks) followed by
// audio and video moof/mdat pairs. Each client gets its own DASHStream with
// independent upstream connections.
type DASHStream struct {
	pipe      *stream.Pipe
	done      chan struct{}
	closeErr  error
	closeOnce sync.Once
	hc        *http.Client
	extractFn stream.ExtractFunc

	// refreshCh signals the produce loop to re-extract before the URL expires.
	refreshCh chan struct{}
}

// trackState tracks the fetch progress of one representation.
type trackState struct {
	name     string
	baseURL  string
	sq       int // next segment sequence to fetch
	notFound int // consecutive 404 batches
}

// tfdtCursor remembers the highest baseMediaDecodeTime already piped per
// track. It lives outside trackState so it survives re-extraction, which
// rebuilds the tracks but must not rewind the stream.
type tfdtCursor struct {
	value uint64
	seen  bool
}

// NewDASHStream creates a DASHStream and starts its produce loop.
func NewDASHStream(extractFn stream.ExtractFunc, hc *http.Client) *DASHStream {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.NewDASHStream")
	log.Debug("creating DASHStream")
	s := &DASHStream{
		pipe:      stream.NewPipe(),
		done:      make(chan struct{}),
		hc:        hc,
		extractFn: extractFn,
		refreshCh: make(chan struct{}, 1),
	}
	go s.produce()
	return s
}

func (s *DASHStream) Read(p []byte) (int, error) {
	return s.pipe.Read(p)
}

func (s *DASHStream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.Close")
	log.Debug("closing DASHStream")
	s.closeOnce.Do(func() {
		s.pipe.BreakWithError(io.ErrClosedPipe)
		close(s.done)
	})
	return nil
}

func (s *DASHStream) Wait() error {
	<-s.done
	return s.closeErr
}

func (s *DASHStream) closeWithError(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.closeWithError")
	log.Warnf("closing DASHStream with error: %s", err.Error())
	s.closeErr = err
	s.pipe.CloseWithError(err)
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

func (s *DASHStream) produce() {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.produce")

	var previous *stream.ExtractResult
	var audio, video *trackState
	var audioCursor, videoCursor tfdtCursor
	var currentHeaders http.Header
	var initPiped bool

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
			leadTime = max(remaining-5*time.Second, 0)
		}
		when := max(remaining-leadTime, 0)
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

		// Extract phase: resolve the MPD and set up both tracks.
		if audio == nil || video == nil {
			result, err := s.extractFn(previous)
			if err != nil {
				if errors.Is(err, stream.ErrFormatDeadlock) {
					log.Errorf("extract format deadlock, closing stream: %s", err.Error())
					s.closeWithError(err)
					return
				}
				log.Warnf("extract error: %s", err.Error())
				time.Sleep(2 * time.Second)
				continue
			}
			previous = result
			currentHeaders = result.Headers
			info, err := fetchAndParseMPD(s.hc, result.URL, result.Headers)
			if err != nil {
				log.Warnf("MPD fetch error: %s", err.Error())
				time.Sleep(2 * time.Second)
				continue
			}
			audioRep, videoRep, err := pickRepresentations(info)
			if err != nil {
				log.Warnf("representation pick error: %s", err.Error())
				time.Sleep(2 * time.Second)
				continue
			}
			audio = &trackState{name: "audio", baseURL: audioRep.BaseURL, sq: max(audioRep.LastSQ-startBackoff, audioRep.FirstSQ)}
			video = &trackState{name: "video", baseURL: videoRep.BaseURL, sq: max(videoRep.LastSQ-startBackoff, videoRep.FirstSQ)}
			// tfdt cursors and initPiped survive re-extraction so the
			// client stream never rewinds; a fresh merged init is piped
			// only when one has never been sent.
			log.Debugf("tracks ready: audio itag=%s sq=%d, video itag=%s sq=%d", audioRep.ID, audio.sq, videoRep.ID, video.sq)
			scheduleRefresh(result.ExpireAt)
			continue
		}

		// First data round: probe both inits with lightweight requests —
		// each response is closed as soon as its first moof arrives, so the
		// multi-megabyte media payload is never fetched — then merge the
		// inits and pipe the single combined init.
		if !initPiped {
			aInit, aErr := s.fetchInitOnly(audio, currentHeaders)
			var vInit []byte
			var vErr error
			if aErr == nil {
				vInit, vErr = s.fetchInitOnly(video, currentHeaders)
			}
			var merged []byte
			var mErr error
			if aErr == nil && vErr == nil {
				merged, mErr = mergeInits(aInit, vInit)
			}
			if aErr != nil || vErr != nil || mErr != nil {
				for _, e := range []error{aErr, vErr, mErr} {
					if e == nil {
						continue
					}
					if isExpiredDASH(e) || mErr != nil {
						log.Warnf("init probe error, re-extracting: %s", e.Error())
						audio, video = nil, nil
					} else {
						log.Warnf("init probe error, retrying: %s", e.Error())
						time.Sleep(pollInterval)
					}
					break
				}
				continue
			}
			if _, err := s.pipe.Write(merged); err != nil {
				return
			}
			initPiped = true
			log.Infoln("piped merged init")
		}

		// Stream one batch per track, box-by-box: audio keeps track_ID 1,
		// video is rewritten to 2, and moofs at or below the track's tfdt
		// cursor are skipped (batches overlap: each starts at the requested
		// sq and runs to the live edge).
		if err := s.pipeTrackBatch(audio, &audioCursor, false, currentHeaders); err != nil {
			if isExpiredDASH(err) {
				log.Warnf("audio batch 403, re-extracting: %s", err.Error())
				audio, video = nil, nil
				continue
			}
			audio.notFound++
			if audio.notFound >= notFoundLimit {
				log.Warnf("audio batch failing %d times, re-extracting: %s", audio.notFound, err.Error())
				audio, video = nil, nil
				continue
			}
			log.Debugf("audio batch error, retrying same sq: %s", err.Error())
			time.Sleep(pollInterval)
			continue
		}
		audio.notFound = 0
		if err := s.pipeTrackBatch(video, &videoCursor, true, currentHeaders); err != nil {
			if isExpiredDASH(err) {
				log.Warnf("video batch 403, re-extracting: %s", err.Error())
				audio, video = nil, nil
				continue
			}
			video.notFound++
			if video.notFound >= notFoundLimit {
				log.Warnf("video batch failing %d times, re-extracting: %s", video.notFound, err.Error())
				audio, video = nil, nil
				continue
			}
			log.Debugf("video batch error, retrying same sq: %s", err.Error())
			time.Sleep(pollInterval)
			continue
		}
		video.notFound = 0

		if s.pipe.Err() != nil {
			return
		}

		// Wait before the next poll, but wake early if the token needs refreshing.
		select {
		case <-s.refreshCh:
			log.Infoln("token refresh triggered, re-extracting")
			audio, video = nil, nil
		case <-time.After(pollInterval):
		}
	}
}

// doTrackRequest issues a GET for the track's current sequence number.
func (s *DASHStream) doTrackRequest(t *trackState, headers http.Header) (*http.Response, error) {
	u := t.baseURL + "sq/" + strconv.Itoa(t.sq) + "/lmt/1"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s request sq=%d: %w", t.name, t.sq, err)
	}
	for k := range headers {
		req.Header.Set(k, headers.Get(k))
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s sq=%d: %w", t.name, t.sq, err)
	}
	return resp, nil
}

// fetchInitOnly issues a lightweight request at the track's current sequence
// and returns just the init part (everything before the first moof: ftyp +
// moov + emsg). The response is closed as soon as the first moof arrives, so
// the multi-megabyte media payload is never downloaded. The sequence is not
// advanced.
func (s *DASHStream) fetchInitOnly(t *trackState, headers http.Header) ([]byte, error) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.fetchInitOnly")
	resp, err := s.doTrackRequest(t, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s init sq=%d got status %d", t.name, t.sq, resp.StatusCode)
	}
	br := newBoxReader(resp.Body)
	var init []byte
	for {
		box, typ, err := br.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s init sq=%d: %w", t.name, t.sq, err)
		}
		if typ == "moof" {
			break
		}
		init = append(init, box...)
	}
	log.Debugf("probed %s init at sq=%d (%d bytes)", t.name, t.sq, len(init))
	return init, nil
}

// pipeTrackBatch streams one batch response into the pipe box-by-box: the
// repeated init part (everything before the first moof) is skipped, moofs at
// or below the track's tfdt cursor are dropped, and video moofs get their
// tfhd track_ID rewritten 1→2. Boxes following a moof (mdat, emsg) inherit
// its decision. The sequence advances only when the response completes
// cleanly, so an interrupted batch is re-fetched from the same sq and
// deduplicated by tfdt.
func (s *DASHStream) pipeTrackBatch(t *trackState, cursor *tfdtCursor, video bool, headers http.Header) error {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.pipeTrackBatch")
	resp, err := s.doTrackRequest(t, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s batch sq=%d got status %d", t.name, t.sq, resp.StatusCode)
	}
	br := newBoxReader(resp.Body)
	seenMoof := false
	pipeCurrent := false
	piped := 0
	for {
		box, typ, err := br.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s batch sq=%d: %w", t.name, t.sq, err)
		}
		if !seenMoof && typ != "moof" {
			continue // repeated init part of this batch
		}
		seenMoof = true
		if typ == "moof" {
			tfdt, ok := moofTfdt(box, 0, len(box))
			pipeCurrent = !(ok && cursor.seen && tfdt <= cursor.value)
			if pipeCurrent {
				if video {
					rewriteMoofTrackIDs(box, 0, len(box), 1, 2)
				}
				if ok {
					cursor.value = tfdt
					cursor.seen = true
				}
				piped++
			}
		}
		if pipeCurrent {
			if _, err := s.pipe.Write(box); err != nil {
				return err
			}
		}
	}
	t.sq++
	log.Debugf("streamed %s batch sq=%d (%d moofs, next sq=%d)", t.name, t.sq-1, piped, t.sq)
	return nil
}

// isExpiredDASH reports whether the error indicates the URLs went stale
// (403) and re-extraction is needed.
func isExpiredDASH(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 403")
}
