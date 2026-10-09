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

		// Fetch one batch per track.
		aBatch, err := s.fetchBatch(audio, currentHeaders)
		if err != nil {
			if isExpiredDASH(err) {
				log.Warnf("audio batch 403, re-extracting: %s", err.Error())
				audio, video = nil, nil
				continue
			}
			audio.notFound++
			if audio.notFound >= notFoundLimit {
				log.Warnf("audio batch missing %d times, re-extracting: %s", audio.notFound, err.Error())
				audio, video = nil, nil
				continue
			}
			log.Debugf("audio batch not ready, retrying same sq: %s", err.Error())
			time.Sleep(pollInterval)
			continue
		}
		audio.notFound = 0
		vBatch, err := s.fetchBatch(video, currentHeaders)
		if err != nil {
			if isExpiredDASH(err) {
				log.Warnf("video batch 403, re-extracting: %s", err.Error())
				audio, video = nil, nil
				continue
			}
			video.notFound++
			if video.notFound >= notFoundLimit {
				log.Warnf("video batch missing %d times, re-extracting: %s", video.notFound, err.Error())
				audio, video = nil, nil
				continue
			}
			log.Debugf("video batch not ready, retrying same sq: %s", err.Error())
			time.Sleep(pollInterval)
			continue
		}
		video.notFound = 0

		// First data round: pipe the merged init (audio ftyp + moov with
		// both traks). On re-extraction the init is piped again only if it
		// never made it out (standard fMP4 stream restart semantics).
		if !initPiped {
			aInitEnd := firstMoofOffset(aBatch)
			vInitEnd := firstMoofOffset(vBatch)
			if aInitEnd < 0 || vInitEnd < 0 {
				log.Warnln("batch without moof, re-extracting")
				audio, video = nil, nil
				continue
			}
			merged, err := mergeInits(aBatch[:aInitEnd], vBatch[:vInitEnd])
			if err != nil {
				log.Warnf("merge inits error: %s", err.Error())
				audio, video = nil, nil
				continue
			}
			if _, err := s.pipe.Write(merged); err != nil {
				return
			}
			initPiped = true
			log.Infoln("piped merged init")
		}

		// Pipe media moofs: audio keeps track_ID 1, video is rewritten to 2.
		// Batches overlap the previous ones (each starts at the requested
		// sq and runs to the live edge), so moofs at or below the cursor's
		// tfdt are skipped.
		s.pipeTrackMoofs(aBatch, audio, &audioCursor, false)
		s.pipeTrackMoofs(vBatch, video, &videoCursor, true)

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

// fetchBatch downloads the batch response starting at the track's current
// sequence number (init + every generated moof up to the live edge) and
// advances the sequence.
func (s *DASHStream) fetchBatch(t *trackState, headers http.Header) ([]byte, error) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.fetchBatch")
	u := t.baseURL + "sq/" + strconv.Itoa(t.sq) + "/lmt/1"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s batch request sq=%d: %w", t.name, t.sq, err)
	}
	for k := range headers {
		req.Header.Set(k, headers.Get(k))
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s batch sq=%d: %w", t.name, t.sq, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s batch sq=%d got status %d", t.name, t.sq, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s batch sq=%d: %w", t.name, t.sq, err)
	}
	t.sq++
	log.Debugf("fetched %s batch sq=%d (%d bytes, next sq=%d)", t.name, t.sq-1, len(body), t.sq)
	return body, nil
}

// pipeTrackMoofs writes the moof groups of a batch into the pipe, skipping
// duplicates by tfdt. video batches get their tfhd track_ID rewritten 1->2.
func (s *DASHStream) pipeTrackMoofs(batch []byte, t *trackState, cursor *tfdtCursor, video bool) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.DASHStream.pipeTrackMoofs")
	var moofs []boxInfo
	for _, b := range walkBoxes(batch, 0, len(batch)) {
		if b.typ == "moof" {
			moofs = append(moofs, b)
		}
	}
	piped := 0
	for i, m := range moofs {
		end := len(batch)
		if i+1 < len(moofs) {
			end = moofs[i+1].off
		}
		tfdt, ok := moofTfdt(batch, m.off, m.size)
		if ok && cursor.seen && tfdt <= cursor.value {
			continue
		}
		if video {
			rewriteMoofTrackIDs(batch, m.off, end-m.off, 1, 2)
		}
		if _, err := s.pipe.Write(batch[m.off:end]); err != nil {
			return
		}
		if ok {
			cursor.value = tfdt
			cursor.seen = true
		}
		piped++
	}
	if piped > 0 {
		log.Debugf("piped %d %s moofs (tfdt cursor %d)", piped, t.name, cursor.value)
	}
}

// isExpiredDASH reports whether the error indicates the URLs went stale
// (403) and re-extraction is needed.
func isExpiredDASH(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 403")
}
