package bmff

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ISO-BMFF (fMP4) box helpers shared by the forwarders that remux ISO
// track streams (the DASH and SABR forwarders both consume YouTube's
// ftyp + moov + (moof + mdat)* batch responses and interleave two tracks
// into one stream).
//
// YouTube live batches repeat the init on every response, so batches after
// the first must be stripped back to their first moof. For the interleaved
// two-track output both init moovs are merged (video track renumbered to 2)
// and every video moof gets its tfhd track_ID rewritten from 1 to 2; both
// are plain 4-byte field writes, no re-encoding.

// BoxInfo describes a single ISO-BMFF box inside a byte slice.
type BoxInfo struct {
	Typ  string
	Off  int
	Size int
}

// WalkBoxes lists the top-level boxes in b[off:end].
func WalkBoxes(b []byte, off, end int) []BoxInfo {
	var out []BoxInfo
	for off+8 <= end && off+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[off : off+4]))
		typ := string(b[off+4 : off+8])
		if size == 0 {
			size = end - off
		} else if size == 1 && off+16 <= len(b) {
			size = int(binary.BigEndian.Uint64(b[off+8 : off+16]))
		}
		if size < 8 || off+size > end {
			break
		}
		out = append(out, BoxInfo{typ, off, size})
		off += size
	}
	return out
}

// FindBox returns the first box of the given type in b[off:end], or nil.
func FindBox(b []byte, off, end int, typ string) *BoxInfo {
	boxes := WalkBoxes(b, off, end)
	for i := range boxes {
		if boxes[i].Typ == typ {
			return &boxes[i]
		}
	}
	return nil
}

// WalkBoxesPartial lists the complete top-level boxes in b and returns how
// many bytes they span. b[consumed:] is the tail of a box that is still
// arriving — or garbage, which the caller cannot distinguish until more
// data lands. It is the incremental companion of WalkBoxes for streaming
// input: feed it the accumulated buffer, consume the returned boxes, keep
// the rest, and call again with the rest plus the next chunk.
func WalkBoxesPartial(b []byte) (boxes []BoxInfo, consumed int) {
	off := 0
	for off+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[off : off+4]))
		if size == 0 {
			// Box extends to the end of the stream: it is complete only
			// when it is the last thing in the buffer.
			return boxes, len(b)
		}
		if size == 1 {
			if off+16 > len(b) {
				return boxes, off
			}
			size = int(binary.BigEndian.Uint64(b[off+8 : off+16]))
		}
		if size < 8 {
			// A size below the header length cannot resolve even with more
			// data: stop and let the caller decide.
			return boxes, off
		}
		if off+size > len(b) {
			return boxes, off
		}
		boxes = append(boxes, BoxInfo{string(b[off+4 : off+8]), off, size})
		off += size
	}
	return boxes, off
}

// RewriteTrakTrackID changes the tkhd track_ID inside a trak box (in place).
func RewriteTrakTrackID(b []byte, trakOff, trakSize int, newID uint32) error {
	tkhd := FindBox(b, trakOff+8, trakOff+trakSize, "tkhd")
	if tkhd == nil || tkhd.Size < 8+16 {
		return fmt.Errorf("tkhd not found or too small in trak at %d", trakOff)
	}
	binary.BigEndian.PutUint32(b[tkhd.Off+8+12:], newID)
	return nil
}

// RewriteMoofTrackIDs rewrites tfhd track_ID values equal to from to to
// inside a moof (in place). Returns the number of rewrites.
func RewriteMoofTrackIDs(b []byte, moofOff, moofSize int, from, to uint32) int {
	n := 0
	for _, traf := range WalkBoxes(b, moofOff+8, moofOff+moofSize) {
		if traf.Typ != "traf" {
			continue
		}
		for _, tf := range WalkBoxes(b, traf.Off+8, traf.Off+traf.Size) {
			if tf.Typ != "tfhd" || tf.Size < 8+8 {
				continue
			}
			p := tf.Off + 8 + 4
			if binary.BigEndian.Uint32(b[p:p+4]) == from {
				binary.BigEndian.PutUint32(b[p:p+4], to)
				n++
			}
		}
	}
	return n
}

// MoofTfdt returns the baseMediaDecodeTime of the first traf's tfdt in a
// moof. ok is false when the moof carries no tfdt.
func MoofTfdt(b []byte, moofOff, moofSize int) (uint64, bool) {
	for _, traf := range WalkBoxes(b, moofOff+8, moofOff+moofSize) {
		if traf.Typ != "traf" {
			continue
		}
		for _, tf := range WalkBoxes(b, traf.Off+8, traf.Off+traf.Size) {
			if tf.Typ != "tfdt" || tf.Size < 8+8 {
				continue
			}
			if tf.Size >= 8+12 && b[tf.Off+8] == 1 {
				return binary.BigEndian.Uint64(b[tf.Off+8+4 : tf.Off+8+12]), true
			}
			return uint64(binary.BigEndian.Uint32(b[tf.Off+8+4 : tf.Off+8+8])), true
		}
	}
	return 0, false
}

// BoxReader incrementally reads top-level ISO-BMFF boxes from a stream so a
// live batch response can be piped box-by-box while it downloads, instead of
// waiting for the whole (multi-megabyte) batch to arrive.
type BoxReader struct {
	r   io.Reader
	hdr [8]byte
}

// NewBoxReader wraps r.
func NewBoxReader(r io.Reader) *BoxReader {
	return &BoxReader{r: r}
}

// Next reads one complete top-level box and returns its bytes (header
// included). Returns io.EOF at a clean end of stream.
func (br *BoxReader) Next() (box []byte, typ string, err error) {
	if _, err := io.ReadFull(br.r, br.hdr[:]); err != nil {
		return nil, "", err
	}
	size := int64(binary.BigEndian.Uint32(br.hdr[0:4]))
	typ = string(br.hdr[4:8])
	if size == 0 {
		// Box extends to the end of the stream.
		rest, err := io.ReadAll(br.r)
		if err != nil {
			return nil, "", err
		}
		out := make([]byte, 0, 8+len(rest))
		out = append(out, br.hdr[:]...)
		out = append(out, rest...)
		return out, typ, nil
	}
	if size == 1 {
		// 64-bit largesize follows the header.
		var ext [8]byte
		if _, err := io.ReadFull(br.r, ext[:]); err != nil {
			return nil, "", err
		}
		size = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if size < 8 {
		return nil, "", fmt.Errorf("bad box size %d for %s", size, typ)
	}
	out := make([]byte, size)
	copy(out, br.hdr[:])
	if _, err := io.ReadFull(br.r, out[8:]); err != nil {
		return nil, "", err
	}
	return out, typ, nil
}

// RewriteTrexTrackID changes the track_ID inside a trex box (in place).
func RewriteTrexTrackID(b []byte, trexOff, trexSize int, newID uint32) error {
	if trexSize < 8+8 {
		return fmt.Errorf("trex at %d too small", trexOff)
	}
	binary.BigEndian.PutUint32(b[trexOff+8+4:], newID)
	return nil
}

// BuildBox serializes a box with the given type and payload.
func BuildBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// MergeInits merges the audio and video init parts (each ftyp + moov + emsg…)
// into one init for the interleaved stream: the audio ftyp, a single moov
// holding the audio trak (track_ID 1) and the renumbered video trak
// (track_ID 2, mvhd next_track_ID 3), then both emsg boxes. The video
// mvex/trex is renumbered to track_ID 2 and appended to the audio mvex —
// without a trex per track, strict demuxers (ffmpeg, PotPlayer) reject the
// fragments ("could not find corresponding trex"). Neither input is modified.
func MergeInits(audioInit, videoInit []byte) ([]byte, error) {
	aFtyp := FindBox(audioInit, 0, len(audioInit), "ftyp")
	aMoov := FindBox(audioInit, 0, len(audioInit), "moov")
	vMoov := FindBox(videoInit, 0, len(videoInit), "moov")
	if aFtyp == nil || aMoov == nil {
		return nil, fmt.Errorf("audio init lacks ftyp/moov")
	}
	if FindBox(audioInit, aMoov.Off+8, aMoov.Off+aMoov.Size, "trak") == nil {
		return nil, fmt.Errorf("audio moov lacks trak")
	}
	if vMoov == nil {
		return nil, fmt.Errorf("video init lacks moov")
	}
	vTrak := FindBox(videoInit, vMoov.Off+8, vMoov.Off+vMoov.Size, "trak")
	if vTrak == nil {
		return nil, fmt.Errorf("video moov lacks trak")
	}
	vMvex := FindBox(videoInit, vMoov.Off+8, vMoov.Off+vMoov.Size, "mvex")
	if vMvex == nil {
		return nil, fmt.Errorf("video moov lacks mvex")
	}
	vTrex := FindBox(videoInit, vMvex.Off+8, vMvex.Off+vMvex.Size, "trex")
	if vTrex == nil {
		return nil, fmt.Errorf("video mvex lacks trex")
	}

	// Clone the buffers so the inputs stay untouched; offsets in the clones
	// match the originals.
	a := append([]byte(nil), audioInit...)
	v := append([]byte(nil), videoInit...)
	if err := RewriteTrakTrackID(v, vTrak.Off, vTrak.Size, 2); err != nil {
		return nil, fmt.Errorf("rewrite video trak id: %w", err)
	}
	if err := RewriteTrexTrackID(v, vTrex.Off, vTrex.Size, 2); err != nil {
		return nil, fmt.Errorf("rewrite video trex id: %w", err)
	}
	mvhd := FindBox(a, aMoov.Off+8, aMoov.Off+aMoov.Size, "mvhd")
	if mvhd == nil || mvhd.Size < 8+100 {
		return nil, fmt.Errorf("audio mvhd not found or too small")
	}
	binary.BigEndian.PutUint32(a[mvhd.Off+8+96:], 3) // next_track_ID
	aMvex := FindBox(a, aMoov.Off+8, aMoov.Off+aMoov.Size, "mvex")
	if aMvex == nil {
		return nil, fmt.Errorf("audio moov lacks mvex")
	}

	// Rebuild the merged moov: every audio moov box with the mvex payload
	// extended by the renumbered video trex, then the video trak appended.
	var moovContent []byte
	for _, bx := range WalkBoxes(a, aMoov.Off+8, aMoov.Off+aMoov.Size) {
		if bx.Typ == "mvex" {
			mvexPayload := make([]byte, 0, bx.Size-8+vTrex.Size)
			mvexPayload = append(mvexPayload, a[bx.Off+8:bx.Off+bx.Size]...)
			mvexPayload = append(mvexPayload, v[vTrex.Off:vTrex.Off+vTrex.Size]...)
			moovContent = append(moovContent, BuildBox("mvex", mvexPayload)...)
		} else {
			moovContent = append(moovContent, a[bx.Off:bx.Off+bx.Size]...)
		}
	}
	moovContent = append(moovContent, v[vTrak.Off:vTrak.Off+vTrak.Size]...)
	mergedMoov := BuildBox("moov", moovContent)

	out := make([]byte, 0, len(a)+len(v))
	out = append(out, a[aFtyp.Off:aFtyp.Off+aFtyp.Size]...)
	out = append(out, mergedMoov...)
	for _, src := range [][]byte{a, v} {
		for _, bx := range WalkBoxes(src, 0, len(src)) {
			if bx.Typ == "emsg" {
				out = append(out, src[bx.Off:bx.Off+bx.Size]...)
			}
		}
	}
	return out, nil
}
