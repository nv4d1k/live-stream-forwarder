package ebml

import (
	"encoding/binary"
	"errors"
)

// EBML (Matroska/WebM) parsing: variable-length integers, element scanning
// and an incremental WebM demuxer. The demuxer targets YouTube's live SABR
// video payloads — one self-contained WebM document per round (EBML header,
// Segment{Info, Tracks, Cluster{Timecode, SimpleBlock…}}) — but stays a
// general element walker so container variations degrade gracefully.
//
// An EBML vint's byte count is the number of leading zero bits plus one; the
// first 1 bit is the marker and is dropped for value semantics (sizes,
// numbers) and kept for element IDs. Unknown sizes (all value bits set) mark
// open-ended masters — YouTube's live Segment and its single big Cluster
// both use them, so their children are scanned as siblings at the current
// depth with a small state machine.

// Common element IDs used by the demuxer.
const (
	idEBMLHeader      = 0x1A45DFA3
	idSegment         = 0x18538067
	idInfo            = 0x1549A966
	idTimecodeScale   = 0x2AD7B1
	idTracks          = 0x1654AE6B
	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackType       = 0x83
	idCodecID         = 0x86
	idVideo           = 0xE0
	idPixelWidth      = 0xB0
	idPixelHeight     = 0xBA
	idDisplayWidth    = 0x55B0
	idDisplayHeight   = 0x55BA
	idCluster         = 0x1F43B675
	idClusterTimecode = 0xE7
	idSimpleBlock     = 0xA3
	idBlockGroup      = 0xA0
	idBlock           = 0xA1
)

// AppendVint appends the EBML vint encoding of v (marker bit added). An
// n-byte vint carries 8-n value bits in its first byte and 8 in each of
// the n-1 following bytes, so the first byte's shift is 8*(n-1). The all-
// ones value of every length is reserved for "unknown size", so an n-byte
// vint only encodes up to 2^(7n)-2 and 127 must widen to two bytes.
func AppendVint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<7-1:
		return append(b, byte(v)|0x80)
	case v < 1<<14-1:
		return append(b, byte(v>>8)|0x40, byte(v))
	case v < 1<<21-1:
		return append(b, byte(v>>16)|0x20, byte(v>>8), byte(v))
	case v < 1<<28-1:
		return append(b, byte(v>>24)|0x10, byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<35-1:
		return append(b, byte(v>>32)|0x08, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<42-1:
		return append(b, byte(v>>40)|0x04, byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<49-1:
		return append(b, byte(v>>48)|0x02, byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, byte(v>>56)|0x01, byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// AppendID appends an element ID's canonical byte form.
func AppendID(b []byte, id uint64) []byte {
	n := IDLen(id)
	for shift := (n - 1) * 8; shift >= 0; shift -= 8 {
		b = append(b, byte(id>>uint(shift)))
	}
	return b
}

// IDLen returns the encoded length of an element ID: the leading-zero count
// of its highest nonzero byte plus one.
func IDLen(id uint64) int {
	n := 1
	for id>>8 != 0 {
		id >>= 8
		n++
	}
	return zeroCount(byte(id)) + 1
}

// beUint decodes a big-endian unsigned integer of up to 8 bytes. Most
// Matroska integer payloads (TimecodeScale, PixelWidth, cluster timecodes…)
// are fixed-length BE values whose length is the element size, not vints.
func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// zeroCount counts leading zero bits of a byte; 0 returns 8.
func zeroCount(b byte) int {
	n := 0
	for b&0x80 == 0 {
		b <<= 1
		n++
		if n == 8 {
			return 8
		}
	}
	return n
}

// Vint decodes a value vint from b at off. ok is false when the vint is not
// fully buffered yet. The marker bit contributes nothing to the value.
func Vint(b []byte, off int) (val uint64, next int, ok bool) {
	if off >= len(b) {
		return 0, off, false
	}
	n := zeroCount(b[off]) + 1
	if off+n > len(b) {
		return 0, off, false
	}
	var v uint64
	if n < 8 {
		v = uint64(b[off] & (0xFF >> uint(n)))
	} else {
		n = 8 // first byte 0x01: marker only, no value bits
	}
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[off+i])
	}
	return v, off + n, true
}

// IsUnknownSize reports whether a decoded size vint means "all value bits
// set" (open-ended master). An n-byte vint carries 8-n value bits in its
// first byte plus 8 per following byte: 7n-1 bits total.
func IsUnknownSize(val uint64, byteLen int) bool {
	all := (uint64(1) << uint(7*byteLen)) - 1
	return val == all
}

// Elem describes one scanned element inside a buffer.
type Elem struct {
	ID       uint64
	DataOff  int
	DataSize int // -1 when unknown (open-ended)
	End      int // one past the element; invalid when DataSize < 0
}

// ErrCorrupt marks a stream that can never parse (bad ID/size combination
// or a truncated master that will not grow).
var ErrCorrupt = errors.New("ebml: corrupt element stream")

// ScanElem scans one element header at b[off]. It returns the element, the
// offset of the next element, and needMore=true when the header (or, for
// known sizes, the payload) is not fully buffered yet. Unknown-size masters
// return immediately with DataSize -1; their children are scanned by the
// caller at the next offset.
func ScanElem(b []byte, off int) (e Elem, next int, needMore bool, err error) {
	if off >= len(b) {
		return e, off, true, nil
	}
	// Element ID: vint-shaped, but the value keeps the marker bit.
	idFirst := b[off]
	idLenBytes := zeroCount(idFirst) + 1
	if off+idLenBytes > len(b) {
		return e, off, true, nil
	}
	if idLenBytes > 8 || idFirst == 0 {
		return e, off, false, ErrCorrupt
	}
	var id uint64
	for i := 0; i < idLenBytes; i++ {
		id = id<<8 | uint64(b[off+i])
	}
	sizeVal, p, ok := Vint(b, off+idLenBytes)
	if !ok {
		return e, off, true, nil
	}
	sizeLen := p - (off + idLenBytes)
	if IsUnknownSize(sizeVal, sizeLen) {
		return Elem{ID: id, DataOff: p, DataSize: -1}, p, false, nil
	}
	if p+int(sizeVal) > len(b) {
		return e, off, true, nil
	}
	e = Elem{ID: id, DataOff: p, DataSize: int(sizeVal), End: p + int(sizeVal)}
	return e, e.End, false, nil
}

// ScanSimpleBlock parses a SimpleBlock/Block payload:
// track vint, int16 relative timecode, flags byte, frame data.
func ScanSimpleBlock(payload []byte) (track uint64, relMs int16, keyframe bool, frame []byte, ok bool) {
	track, p, ok := Vint(payload, 0)
	if !ok || p+3 > len(payload) {
		return 0, 0, false, nil, false
	}
	relMs = int16(binary.BigEndian.Uint16(payload[p : p+2]))
	flags := payload[p+2]
	return track, relMs, flags&0x80 != 0, payload[p+3:], true
}

// ---- incremental WebM demuxer ----

// Frame is one demuxed video frame with its absolute (document-time) ms
// timecode.
type Frame struct {
	TimecodeMs int64
	Keyframe   bool
	Data       []byte
}

// WebM incrementally demuxes a live WebM document, emitting video frames
// with absolute millisecond timecodes (cluster timecode + signed 16-bit
// block offset). Feed it the document's bytes in arrival order; frames are
// copied out, so the caller may reuse the input buffer after each call.
type WebM struct {
	buf []byte

	// document metadata (from Info/Tracks)
	timecodeScale      uint64 // ns per tick; 1000000 (1ms) on YouTube live
	trackNumber        uint64
	codecID            string
	width, height      int
	displayW, displayH int

	// parse state
	clusterTC   int64
	openCluster bool // inside an unknown-size Cluster
	metaSeen    bool
}

// NewWebM creates a demuxer for one WebM document.
func NewWebM() *WebM {
	return &WebM{timecodeScale: 1000000}
}

// CodecID returns the video codec ID parsed from Tracks ("V_VP9").
func (d *WebM) CodecID() string { return d.codecID }

// VideoSize returns the pixel dimensions parsed from Tracks.
func (d *WebM) VideoSize() (w, h int) { return d.width, d.height }

// TrackNumber returns the video track number declared in the document.
func (d *WebM) TrackNumber() uint64 { return d.trackNumber }

// MetaSeen reports whether Info and Tracks have been consumed.
func (d *WebM) MetaSeen() bool { return d.metaSeen }

// Feed appends document bytes and returns the frames completed by this
// chunk. Unknown-size masters (Segment, and YouTube's live Cluster) extend
// to the end of everything fed so far, so their children are scanned as
// siblings at the current depth and tracked with a small state machine.
func (d *WebM) Feed(b []byte) ([]Frame, error) {
	d.buf = append(d.buf, b...)
	var frames []Frame
	off := 0
	for {
		e, next, needMore, err := ScanElem(d.buf, off)
		if err != nil {
			d.compact(off)
			return frames, err
		}
		if needMore {
			break
		}
		if d.openCluster {
			switch e.ID {
			case idCluster:
				// A new cluster starts while an open one is in progress:
				// both are siblings at the Segment level. An open-ended one
				// just keeps child mode active.
				if e.DataSize >= 0 {
					d.openCluster = false
					d.parseCluster(e, &frames)
				}
			case idInfo, idTracks:
				// Back at Segment-level metadata after an open cluster
				// (unusual, but tolerate).
				d.openCluster = false
				d.handleTopLevel(e, &frames)
			default:
				d.handleClusterChild(e, &frames)
			}
			off = next
			continue
		}
		d.handleTopLevel(e, &frames)
		if e.ID == idCluster && e.DataSize < 0 {
			d.openCluster = true
		}
		off = next
	}
	d.compact(off)
	return frames, nil
}

// compact removes consumed bytes from the head of the buffer.
func (d *WebM) compact(consumed int) {
	if consumed == 0 {
		return
	}
	d.buf = append(d.buf[:0], d.buf[consumed:]...)
}

// handleTopLevel processes one element at the Segment level.
func (d *WebM) handleTopLevel(e Elem, frames *[]Frame) {
	switch e.ID {
	case idEBMLHeader, idSegment:
		// Containers; Segment is open-ended in live documents.
	case idInfo:
		d.parseInfo(e)
	case idTracks:
		d.parseTracks(e)
		d.metaSeen = true
	case idCluster:
		// Known-size cluster: parse directly. Open-ended ones are flipped
		// to child mode by Feed.
		if e.DataSize >= 0 {
			d.parseCluster(e, frames)
		}
	default:
		// Anything else (Cues, Tags, void…) is skipped by advancing.
	}
}

// parseInfo reads the TimecodeScale out of the Info element.
func (d *WebM) parseInfo(e Elem) {
	off := e.DataOff
	for off < e.End {
		c, next, needMore, err := ScanElem(d.buf, off)
		if err != nil || needMore {
			return
		}
		if c.ID == idTimecodeScale && c.DataSize > 0 && c.DataSize <= 8 {
			d.timecodeScale = beUint(d.buf[c.DataOff : c.DataOff+c.DataSize])
		}
		off = next
	}
}

// parseTracks reads the first track entry (track number, codec, size).
func (d *WebM) parseTracks(e Elem) {
	off := e.DataOff
	for off < e.End {
		te, next, needMore, err := ScanElem(d.buf, off)
		if err != nil || needMore {
			return
		}
		if te.ID == idTrackEntry {
			d.parseTrackEntry(te)
			// First track entry wins; live webm carries a single video track.
			return
		}
		off = next
	}
}

// parseTrackEntry reads TrackNumber/CodecID/Video from a TrackEntry.
func (d *WebM) parseTrackEntry(te Elem) {
	p := te.DataOff
	for p < te.End {
		c, cnext, needMore, err := ScanElem(d.buf, p)
		if err != nil || needMore {
			return
		}
		switch c.ID {
		case idTrackNumber:
			if c.DataSize > 0 && c.DataSize <= 8 {
				d.trackNumber = beUint(d.buf[c.DataOff : c.DataOff+c.DataSize])
			}
		case idCodecID:
			if c.DataSize > 0 {
				d.codecID = string(d.buf[c.DataOff : c.DataOff+c.DataSize])
			}
		case idVideo:
			d.parseVideoDims(c)
		}
		p = cnext
	}
}

// parseVideoDims reads PixelWidth/PixelHeight (and display sizes) from a
// Video element.
func (d *WebM) parseVideoDims(e Elem) {
	off := e.DataOff
	for off < e.End {
		c, next, needMore, err := ScanElem(d.buf, off)
		if err != nil || needMore {
			return
		}
		if c.DataSize > 0 && c.DataSize <= 8 {
			v := beUint(d.buf[c.DataOff : c.DataOff+c.DataSize])
			switch c.ID {
			case idPixelWidth:
				d.width = int(v)
			case idPixelHeight:
				d.height = int(v)
			case idDisplayWidth:
				d.displayW = int(v)
			case idDisplayHeight:
				d.displayH = int(v)
			}
		}
		off = next
	}
	if d.displayW == 0 && d.width != 0 {
		d.displayW, d.displayH = d.width, d.height
	}
}

// parseCluster reads a Cluster's Timecode and emits its blocks as frames
// with absolute ms timecodes.
func (d *WebM) parseCluster(e Elem, frames *[]Frame) {
	off := e.DataOff
	for off < e.End {
		c, next, needMore, err := ScanElem(d.buf, off)
		if err != nil || needMore {
			return
		}
		d.handleClusterChild(c, frames)
		off = next
	}
}

// handleClusterChild processes one element inside a Cluster (in both
// known-size and open-ended modes).
func (d *WebM) handleClusterChild(e Elem, frames *[]Frame) {
	switch e.ID {
	case idClusterTimecode:
		if e.DataSize > 0 && e.DataSize <= 8 {
			d.clusterTC = int64(beUint(d.buf[e.DataOff : e.DataOff+e.DataSize]))
		}
	case idSimpleBlock:
		d.emitBlock(d.buf[e.DataOff:e.DataOff+e.DataSize], frames)
	case idBlockGroup:
		p := e.DataOff
		for p < e.End {
			g, gnext, needMore, err := ScanElem(d.buf, p)
			if err != nil || needMore {
				return
			}
			if g.ID == idBlock {
				d.emitBlock(d.buf[g.DataOff:g.DataOff+g.DataSize], frames)
			}
			p = gnext
		}
	}
}

// emitBlock converts a block payload into a frame copy with an absolute ms
// timecode. Blocks of unknown tracks are skipped.
func (d *WebM) emitBlock(payload []byte, frames *[]Frame) {
	track, relMs, keyframe, data, ok := ScanSimpleBlock(payload)
	if !ok {
		return
	}
	if d.trackNumber != 0 && track != d.trackNumber {
		return
	}
	scale := d.timecodeScale
	if scale == 0 {
		scale = 1000000
	}
	abs := d.clusterTC*int64(scale)/1000000 + int64(relMs)
	// The frame data is copied: the demuxer buffer is compacted after Feed.
	frame := make([]byte, len(data))
	copy(frame, data)
	*frames = append(*frames, Frame{TimecodeMs: abs, Keyframe: keyframe, Data: frame})
}
