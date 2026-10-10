package mkv

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/ebml"
)

// Matroska muxer for the SABR forwarder: one VP9 video track and one AAC
// audio track, written as a live stream (open Segment, self-delimiting
// Clusters). Frames arrive per track from the upstream demuxers in
// absolute (broadcast) milliseconds; the muxer normalizes them to a zero
// base, dedupes the per-round overlap by timecode, buffers blocks in
// ~5s windows and flushes each window as one time-ordered Cluster —
// block timecodes stay within int16 of their cluster timecode, and the
// two tracks interleave correctly regardless of upstream arrival order.
// Pure byte shuffling: no transcoding.

// Track numbers in the output.
const (
	TrackVideo = 1
	TrackAudio = 2
)

// windowMs is the block-buffering window flushed as one Cluster.
const windowMs = 5000

// ErrNotConfigured marks a Feed/Flush before the header was written.
var ErrNotConfigured = errors.New("mkv: header not written")

// Element IDs used by the writer.
const (
	idEBMLHeader      = 0x1A45DFA3
	idDocType         = 0x4282
	idDocTypeVer      = 0x4287
	idDocTypeRead     = 0x4285
	idEBMLVer         = 0x4286
	idEBMLRead        = 0x42F7
	idEBMLMaxID       = 0x42F2
	idEBMLMaxSize     = 0x42F3
	idSegment         = 0x18538067
	idInfo            = 0x1549A966
	idTimecodeScale   = 0x2AD7B1
	idMuxingApp       = 0x4D80
	idWritingApp      = 0x5741
	idTracks          = 0x1654AE6B
	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackUID        = 0x73C5 // two-byte ID: 0x73 opens with a zero bit
	idTrackType       = 0x83
	idCodecID         = 0x86
	idCodecPrivate    = 0x63A2
	idVideo           = 0xE0
	idPixelWidth      = 0xB0
	idPixelHeight     = 0xBA
	idDisplayWidth    = 0x55B0
	idDisplayHeight   = 0x55BA
	idAudio           = 0xE1
	idSamplingFreq    = 0xB5
	idChannels        = 0x9F
	idCluster         = 0x1F43B675
	idClusterTimecode = 0xE7
	idSimpleBlock     = 0xA3
)

// elem builds ID + size + payload.
func elem(id uint64, payload []byte) []byte {
	out := ebml.AppendID(nil, id)
	out = ebml.AppendVint(out, uint64(len(payload)))
	return append(out, payload...)
}

// elemOpen builds ID + the 8-byte unknown-size vint.
func elemOpen(id uint64) []byte {
	out := ebml.AppendID(nil, id)
	return append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
}

// beInt appends v as a minimal big-endian integer (>=1 byte).
func beInt(b []byte, v int64) []byte {
	n := 1
	for v >= 1<<(8*uint(n)) || v < -(1<<(8*uint(n)-1)) {
		n++
		if n == 8 {
			break
		}
	}
	if v < 0 {
		n = 8
	}
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>uint(8*i)))
	}
	return b
}

// winBlock is one buffered block awaiting its window flush.
type winBlock struct {
	track    int
	absMs    int64
	keyframe bool
	data     []byte
}

// Muxer writes the Matroska stream. Configure via the setters, call
// WriteHeader once, then Feed frames; call Flush at each round's end so
// the current window is not held back.
type Muxer struct {
	w io.Writer

	// track configuration (set before WriteHeader)
	videoWidth, videoHeight int
	audioRate               uint32
	audioChannels           uint16
	audioCSD                []byte

	// header / base state
	headerWritten bool
	baseMs        int64
	haveBase      bool

	// per-track dedup cursors (absolute ms of the last accepted block)
	cursors    [3]int64 // index by track number
	cursorSeen [3]bool

	// window state
	winStart int64
	winEnd   int64
	blocks   []winBlock

	// blocks fed before the header was written (the upstream demuxers
	// finish the audio track's metadata before the video track's)
	preHeader []winBlock
}

// New creates a muxer writing to w.
func New(w io.Writer) *Muxer {
	return &Muxer{w: w}
}

// SetVideo configures the VP9 track dimensions. Must be called before
// WriteHeader.
func (m *Muxer) SetVideo(width, height int) {
	m.videoWidth, m.videoHeight = width, height
}

// SetAudio configures the AAC track. Must be called before WriteHeader.
func (m *Muxer) SetAudio(sampleRate uint32, channels uint16, csd []byte) {
	m.audioRate, m.audioChannels, m.audioCSD = sampleRate, channels, csd
}

// WriteHeader emits the EBML header, the open Segment, Info and Tracks.
// The base timecode is not known yet (it is the first fed frame), so
// clusters start appearing with the first Feed.
func (m *Muxer) WriteHeader() error {
	if m.videoWidth == 0 || m.audioRate == 0 {
		return errors.New("mkv: video or audio configuration missing")
	}
	var out []byte

	var eh []byte
	eh = append(eh, elem(idEBMLVer, []byte{1})...)
	eh = append(eh, elem(idEBMLRead, []byte{1})...)
	eh = append(eh, elem(idEBMLMaxID, []byte{0x04})...)   // 4-byte IDs
	eh = append(eh, elem(idEBMLMaxSize, []byte{0x08})...) // 8-byte sizes
	eh = append(eh, elem(idDocType, []byte("matroska"))...)
	eh = append(eh, elem(idDocTypeVer, []byte{2})...) // v2: the newest ffmpeg/vlc demuxers implement
	eh = append(eh, elem(idDocTypeRead, []byte{2})...)
	out = append(out, elem(idEBMLHeader, eh)...)
	out = append(out, elemOpen(idSegment)...)

	var info []byte
	info = append(info, elem(idTimecodeScale, []byte{0x0F, 0x42, 0x40})...) // 1e6 ns
	info = append(info, elem(idMuxingApp, []byte("live-stream-forwarder"))...)
	info = append(info, elem(idWritingApp, []byte("live-stream-forwarder"))...)
	out = append(out, elem(idInfo, info)...)

	var videoTrack []byte
	videoTrack = append(videoTrack, elem(idTrackNumber, []byte{TrackVideo})...)
	videoTrack = append(videoTrack, elem(idTrackType, []byte{1})...) // video
	videoTrack = append(videoTrack, elem(idCodecID, []byte("V_VP9"))...)
	var dims []byte
	dims = append(dims, elem(idPixelWidth, beInt(nil, int64(m.videoWidth)))...)
	dims = append(dims, elem(idPixelHeight, beInt(nil, int64(m.videoHeight)))...)
	videoTrack = append(videoTrack, elem(idVideo, dims)...)

	var audioTrack []byte
	audioTrack = append(audioTrack, elem(idTrackNumber, []byte{TrackAudio})...)
	audioTrack = append(audioTrack, elem(idTrackType, []byte{2})...) // audio
	audioTrack = append(audioTrack, elem(idCodecID, []byte("A_AAC"))...)
	var freq [8]byte
	binary.BigEndian.PutUint64(freq[:], math.Float64bits(float64(m.audioRate)))
	var ad []byte
	ad = append(ad, elem(idSamplingFreq, freq[:])...)
	ad = append(ad, elem(idChannels, []byte{byte(m.audioChannels)})...)
	audioTrack = append(audioTrack, elem(idAudio, ad)...)
	if len(m.audioCSD) > 0 {
		audioTrack = append(audioTrack, elem(idCodecPrivate, m.audioCSD)...)
	}

	var tracks []byte
	tracks = append(tracks, elem(idTrackEntry, videoTrack)...)
	tracks = append(tracks, elem(idTrackEntry, audioTrack)...)
	out = append(out, elem(idTracks, tracks)...)

	if _, err := m.w.Write(out); err != nil {
		return err
	}
	m.headerWritten = true
	// Release in time order so the earliest buffered block defines the
	// base: upstream arrival order (video before audio when the audio moov
	// spans parts) would otherwise pin the base to a later frame and
	// produce negative opening timecodes.
	sort.SliceStable(m.preHeader, func(i, j int) bool {
		return m.preHeader[i].absMs < m.preHeader[j].absMs
	})
	for _, b := range m.preHeader {
		if err := m.feed(b.track, b.absMs, b.keyframe, b.data); err != nil {
			return err
		}
	}
	m.preHeader = nil
	return nil
}

// Feed offers one frame. Frames at or before the track's cursor are
// dropped — consecutive SABR rounds overlap ~20s and the dedup keeps the
// client stream monotonic. A frame past the window end flushes the
// buffered window as a Cluster and starts a new one. Frames offered before
// WriteHeader are buffered (with dedup applied) and released once the
// header exists: the upstream demuxers reach the audio metadata before the
// video's, so audio frames legitimately arrive first.
func (m *Muxer) Feed(track int, absMs int64, keyframe bool, data []byte) error {
	if !m.headerWritten {
		if track < 1 || track > 2 {
			return nil
		}
		// Buffer verbatim; the cursors stay untouched so the release pass
		// below runs the real dedup exactly once per block.
		blk := winBlock{track: track, absMs: absMs, keyframe: keyframe, data: make([]byte, len(data))}
		copy(blk.data, data)
		m.preHeader = append(m.preHeader, blk)
		return nil
	}
	return m.feed(track, absMs, keyframe, data)
}

// feed ingests one frame into the live windows; it requires the header.
func (m *Muxer) feed(track int, absMs int64, keyframe bool, data []byte) error {
	if track < 1 || track > 2 {
		return nil
	}
	if m.cursorSeen[track] && absMs <= m.cursors[track] {
		return nil // overlap dedup
	}
	if !m.haveBase {
		m.baseMs = absMs
		m.winStart = absMs
		m.winEnd = absMs + windowMs
		m.haveBase = true
	}
	if absMs >= m.winEnd {
		if err := m.flushWindow(); err != nil {
			return err
		}
		m.winStart = absMs
		m.winEnd = absMs + windowMs
	}
	blk := winBlock{track: track, absMs: absMs, keyframe: keyframe, data: make([]byte, len(data))}
	copy(blk.data, data)
	m.blocks = append(m.blocks, blk)
	m.cursors[track] = absMs
	m.cursorSeen[track] = true
	return nil
}

// Flush closes the current window at a round boundary so the buffered
// blocks are not held back waiting for a future frame.
func (m *Muxer) Flush() error {
	if !m.headerWritten {
		return ErrNotConfigured
	}
	return m.flushWindow()
}

// flushWindow sorts the buffered blocks by timecode, writes them as one
// Cluster and resets the window.
func (m *Muxer) flushWindow() error {
	if len(m.blocks) == 0 {
		return nil
	}
	sort.SliceStable(m.blocks, func(i, j int) bool {
		return m.blocks[i].absMs < m.blocks[j].absMs
	})
	clusterTC := m.blocks[0].absMs - m.baseMs
	if clusterTC < 0 {
		clusterTC = 0
	}
	var payload []byte
	payload = append(payload, elem(idClusterTimecode, beInt(nil, clusterTC))...)
	for _, b := range m.blocks {
		payload = append(payload, simpleBlock(b.track, b.absMs-m.baseMs-clusterTC, b.keyframe, b.data)...)
	}
	if _, err := m.w.Write(elem(idCluster, payload)); err != nil {
		return err
	}
	m.blocks = m.blocks[:0]
	return nil
}

// simpleBlock builds one SimpleBlock element. The timecode is relative to
// the cluster timecode and stays well inside int16 (blocks never leave
// their 5s window).
func simpleBlock(track int, relMs int64, keyframe bool, data []byte) []byte {
	var p []byte
	p = ebml.AppendVint(p, uint64(track))
	p = binary.BigEndian.AppendUint16(p, uint16(int16(relMs)))
	flags := byte(0)
	if keyframe {
		flags = 0x80
	}
	p = append(p, flags)
	p = append(p, data...)
	return elem(idSimpleBlock, p)
}
