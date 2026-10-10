package sabr

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/ebml"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/mkv"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// roundIntervalMs is the pause between two SABR rounds. Each round opens a
// fresh player session (a session serves exactly one request before
// demanding attestation), and each response starts ~20s behind the live
// head — adjacent rounds overlap, so the muxer's per-track timecode dedup
// drops the overlap and no content is lost even with no pacing at all. The
// pause only keeps the innertube API from being hot-looped.
// Package-level atomics so tests can shorten them while earlier tests'
// producer goroutines are still winding down under -race.
var (
	roundIntervalMs atomic.Int64
	retryBackoffMs  atomic.Int64
)

func init() {
	roundIntervalMs.Store(1000)
	retryBackoffMs.Store(2000)
}

// errEmptyRound marks a response that carried neither headers nor media
// (e.g. an attestation rejection: six bytes of StreamProtectionStatus and
// nothing else). Retrying with a fresh session is the only recovery.
var errEmptyRound = errors.New("sabr: response carried no media")

// SABRStream continuously requests YouTube's serverAbrStreamingUrl and
// pipes a two-track Matroska stream to the consumer: a VP9 video track
// (the >1080p itags arrive as WebM documents) and an AAC audio track
// (fMP4). Every round opens a fresh player session and starts ~20s behind
// the live head, so consecutive rounds overlap; the muxer's per-track
// cursors drop the duplicated blocks and the client stream never rewinds.
// Output is a live MKV (open Segment, self-delimiting Clusters) that the
// fan-out hub segments into init + Clusters.
type SABRStream struct {
	pipe      *stream.Pipe
	done      chan struct{}
	closeErr  error
	closeOnce sync.Once
	hc        *http.Client
	extractFn stream.ExtractFunc
	muxer     *mkv.Muxer
}

// NewSABRStream creates a SABRStream and starts its produce loop.
func NewSABRStream(extractFn stream.ExtractFunc, hc *http.Client) *SABRStream {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.NewSABRStream")
	log.Debug("creating SABRStream")
	s := &SABRStream{
		pipe:      stream.NewPipe(),
		done:      make(chan struct{}),
		hc:        hc,
		extractFn: extractFn,
	}
	s.muxer = mkv.New(s.pipe)
	go s.produce()
	return s
}

func (s *SABRStream) Read(p []byte) (int, error) {
	return s.pipe.Read(p)
}

func (s *SABRStream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.SABRStream.Close")
	log.Debug("closing SABRStream")
	s.closeOnce.Do(func() {
		s.pipe.BreakWithError(io.ErrClosedPipe)
		close(s.done)
	})
	return nil
}

func (s *SABRStream) Wait() error {
	<-s.done
	return s.closeErr
}

func (s *SABRStream) closeWithError(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.SABRStream.closeWithError")
	log.Warnf("closing SABRStream with error: %s", err.Error())
	s.closeErr = err
	s.pipe.CloseWithError(err)
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

// produce runs the round loop: fresh extraction, one UMP POST, demux both
// tracks, mux to MKV.
func (s *SABRStream) produce() {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.SABRStream.produce")

	var previous *stream.ExtractResult
	firstRound := true

	for {
		if s.pipe.Err() != nil {
			return
		}

		// Every round needs a fresh player session: a SABR URL serves one
		// request, then the server demands attestation.
		result, err := s.extractFn(previous)
		if err != nil {
			if errors.Is(err, stream.ErrFormatDeadlock) {
				log.Errorf("extract format deadlock, closing stream: %s", err.Error())
				s.closeWithError(err)
				return
			}
			log.Warnf("extract error: %s", err.Error())
			if !s.sleepRound(time.Duration(retryBackoffMs.Load()) * time.Millisecond) {
				return
			}
			continue
		}
		previous = result
		info := result.Sabr
		if info == nil {
			s.closeWithError(errors.New("sabr: extraction returned a non-SABR result"))
			return
		}

		err = s.consumeRound(result, info, &firstRound)
		if err != nil {
			if s.pipe.Err() != nil {
				return
			}
			log.Warnf("round failed: %s", err.Error())
			if !s.sleepRound(time.Duration(retryBackoffMs.Load()) * time.Millisecond) {
				return
			}
			continue
		}
		if s.pipe.Err() != nil {
			return
		}
		if !s.sleepRound(time.Duration(roundIntervalMs.Load()) * time.Millisecond) {
			return
		}
	}
}

// sleepRound waits d or until the stream is closed. Returns false when the
// producer must stop.
func (s *SABRStream) sleepRound(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.done:
		return false
	}
}

// consumeRound performs one SABR exchange: POST one VideoPlaybackAbrRequest
// and demux the response's two media documents into the MKV muxer.
func (s *SABRStream) consumeRound(result *stream.ExtractResult, info *extractor.SabrInfo, firstRound *bool) error {
	log := global.Log.WithField("func", "app.engine.forwarder.sabr.SABRStream.consumeRound")

	resp, err := s.postSabr(result.URL, result.Headers, info)
	if err != nil {
		return fmt.Errorf("post sabr request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sabr response status %d", resp.StatusCode)
	}

	// Each round carries one self-contained document per track: a WebM
	// file for the VP9 itags, an fMP4 for the AAC one.
	videoDemux := ebml.NewWebM()
	audioDemux := NewAACDemuxer()
	tracks := map[byte]bool{} // headerId → isVideo
	sawHeader := false
	sawVideoMeta := false
	sawAudioMeta := false
	mediaBytes := 0
	partCounts := map[int]int{}

	// configureMuxer wires both demuxers' metadata into the muxer and
	// writes the MKV header once both are known.
	configureMuxer := func() error {
		if !sawVideoMeta {
			if w, h := videoDemux.VideoSize(); w > 0 && h > 0 {
				s.muxer.SetVideo(w, h)
				sawVideoMeta = true
			}
		}
		if !sawAudioMeta {
			if _, rate, ch, csd, ok := audioDemux.Meta(); ok && rate > 0 {
				s.muxer.SetAudio(rate, ch, csd)
				sawAudioMeta = true
			}
		}
		if sawVideoMeta && sawAudioMeta && *firstRound {
			_, rate, ch, csd, _ := audioDemux.Meta()
			w, h := videoDemux.VideoSize()
			log.Debugf("muxer config: video %dx%d, audio rate=%d ch=%d csd=%x", w, h, rate, ch, csd)
			if err := s.muxer.WriteHeader(); err != nil {
				return fmt.Errorf("write mkv header: %w", err)
			}
			*firstRound = false
			log.Infoln("wrote MKV header")
		}
		return nil
	}

	buf := make([]byte, 64<<10)
	var pending []byte // incomplete UMP frames
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			mediaBytes += n
			pending = append(pending, buf[:n]...)
			parts, rest, perr := parseUMPParts(pending)
			if perr != nil {
				return perr
			}
			pending = rest
			for _, part := range parts {
				partCounts[part.Type]++
				switch part.Type {
				case umpMediaHeader:
					h, ok := parseMediaHeader(part.Payload)
					if !ok {
						continue
					}
					isVideo := h.ITag == int32(info.VideoITag)
					if !isVideo && h.ITag != int32(info.AudioITag) {
						log.Warnf("unexpected media header itag=%d, ignoring", h.ITag)
						continue
					}
					if !tracks[byte(h.HeaderID)] {
						sawHeader = true
						log.Debugf("%s track: headerId=%d itag=%d seq=%d startMs=%d bitrate=%d",
							map[bool]string{true: "video", false: "audio"}[isVideo],
							h.HeaderID, h.ITag, h.SegmentNum, h.StartMs, h.BitrateBps)
					}
					tracks[byte(h.HeaderID)] = isVideo
				case umpMedia:
					isVideo, known := tracks[byte(part.Payload[0])]
					if !known {
						continue // media for an unexpected header; drop
					}
					data := part.Payload[1:]
					if isVideo {
						frames, ferr := videoDemux.Feed(data)
						if ferr != nil {
							log.Warnf("webm demux error: %s", ferr.Error())
							continue // skip the rest of this document
						}
						if err := configureMuxer(); err != nil {
							return err
						}
						for _, f := range frames {
							if err := s.muxer.Feed(mkv.TrackVideo, f.TimecodeMs, f.Keyframe, f.Data); err != nil {
								return err
							}
						}
					} else {
						frames := audioDemux.Feed(data)
						if err := configureMuxer(); err != nil {
							return err
						}
						for _, f := range frames {
							if err := s.muxer.Feed(mkv.TrackAudio, f.TimecodeMs, false, f.Data); err != nil {
								return err
							}
						}
					}
				case umpStreamProtection:
					status, retries := parseStreamProtectionStatus(part.Payload)
					if status == 3 {
						log.Warnf("stream protection rejected (status=3, retries=%d)", retries)
					} else if status != 0 {
						log.Debugf("stream protection status=%d", status)
					}
				case umpLiveMetadata:
					if headMs, ok := parseLiveMetadata(part.Payload); ok {
						log.Debugf("live head ms=%d", headMs)
					}
				case umpSabrError:
					log.Warnf("sabr error part (%d bytes)", len(part.Payload))
				default:
					// NextRequestPolicy, SelectableFormats, Config and other
					// bookkeeping parts: irrelevant for the fresh-session
					// per-round download model.
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read sabr response: %w", rerr)
		}
		if s.pipe.Err() != nil {
			return s.pipe.Err()
		}
	}

	if !sawHeader {
		log.Debugf("empty round: %d bytes, parts=%v", mediaBytes, partCounts)
		return errEmptyRound
	}
	// Close the muxer's block window so buffered clusters reach subscribers
	// before the next round starts.
	if err := s.muxer.Flush(); err != nil {
		return err
	}
	log.Debugf("round complete: %d bytes, parts=%v", mediaBytes, partCounts)
	return nil
}

// postSabr issues the round's UMP POST to the SABR endpoint.
func (s *SABRStream) postSabr(sabrURL string, headers http.Header, info *extractor.SabrInfo) (*http.Response, error) {
	body := wrapUMPRequest(buildVideoPlaybackAbrRequest(abrRequestParams{
		UstreamerConfig: info.UstreamerConfig,
		VideoITag:       info.VideoITag,
		AudioITag:       info.AudioITag,
		PlayerTimeMs:    liveEdgeSentinelMs,
	}))
	sep := "&"
	if !bytes.ContainsRune([]byte(sabrURL), '?') {
		sep = "?"
	}
	req, err := http.NewRequest(http.MethodPost, sabrURL+sep+"rn=0", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build sabr request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/vnd.yt-ump")
	req.Header.Set("Accept-Encoding", "identity")
	for k := range headers {
		req.Header.Set(k, headers.Get(k))
	}
	return s.hc.Do(req)
}
