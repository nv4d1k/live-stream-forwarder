package dash

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ISO-BMFF (fMP4) box helpers. YouTube live DASH serves each Representation
// as batch responses whose bodies are ftyp + moov + emsg + (moof + mdat)* —
// the init repeats on every batch, so batches after the first must be
// stripped back to their first moof. For the interleaved two-track output
// both init moovs are merged (video track renumbered to 2) and every video
// moof gets its tfhd track_ID rewritten from 1 to 2; both are plain 4-byte
// field writes, no re-encoding.

// boxInfo describes a single ISO-BMFF box inside a byte slice.
type boxInfo struct {
	typ  string
	off  int
	size int
}

// walkBoxes lists the top-level boxes in b[off:end].
func walkBoxes(b []byte, off, end int) []boxInfo {
	var out []boxInfo
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
		out = append(out, boxInfo{typ, off, size})
		off += size
	}
	return out
}

// findBox returns the first box of the given type in b[off:end], or nil.
func findBox(b []byte, off, end int, typ string) *boxInfo {
	boxes := walkBoxes(b, off, end)
	for i := range boxes {
		if boxes[i].typ == typ {
			return &boxes[i]
		}
	}
	return nil
}

// rewriteTrakTrackID changes the tkhd track_ID inside a trak box (in place).
func rewriteTrakTrackID(b []byte, trakOff, trakSize int, newID uint32) error {
	tkhd := findBox(b, trakOff+8, trakOff+trakSize, "tkhd")
	if tkhd == nil || tkhd.size < 8+16 {
		return fmt.Errorf("tkhd not found or too small in trak at %d", trakOff)
	}
	binary.BigEndian.PutUint32(b[tkhd.off+8+12:], newID)
	return nil
}

// rewriteMoofTrackIDs rewrites tfhd track_ID values equal to from to to
// inside a moof (in place). Returns the number of rewrites.
func rewriteMoofTrackIDs(b []byte, moofOff, moofSize int, from, to uint32) int {
	n := 0
	for _, traf := range walkBoxes(b, moofOff+8, moofOff+moofSize) {
		if traf.typ != "traf" {
			continue
		}
		for _, tf := range walkBoxes(b, traf.off+8, traf.off+traf.size) {
			if tf.typ != "tfhd" || tf.size < 8+8 {
				continue
			}
			p := tf.off + 8 + 4
			if binary.BigEndian.Uint32(b[p:p+4]) == from {
				binary.BigEndian.PutUint32(b[p:p+4], to)
				n++
			}
		}
	}
	return n
}

// moofTfdt returns the baseMediaDecodeTime of the first traf's tfdt in a
// moof. ok is false when the moof carries no tfdt.
func moofTfdt(b []byte, moofOff, moofSize int) (uint64, bool) {
	for _, traf := range walkBoxes(b, moofOff+8, moofOff+moofSize) {
		if traf.typ != "traf" {
			continue
		}
		for _, tf := range walkBoxes(b, traf.off+8, traf.off+traf.size) {
			if tf.typ != "tfdt" || tf.size < 8+8 {
				continue
			}
			if tf.size >= 8+12 && b[tf.off+8] == 1 {
				return binary.BigEndian.Uint64(b[tf.off+8+4 : tf.off+8+12]), true
			}
			return uint64(binary.BigEndian.Uint32(b[tf.off+8+4 : tf.off+8+8])), true
		}
	}
	return 0, false
}

// boxReader incrementally reads top-level ISO-BMFF boxes from a stream so a
// live batch response can be piped box-by-box while it downloads, instead of
// waiting for the whole (multi-megabyte) batch to arrive.
type boxReader struct {
	r   io.Reader
	hdr [8]byte
}

func newBoxReader(r io.Reader) *boxReader {
	return &boxReader{r: r}
}

// next reads one complete top-level box and returns its bytes (header
// included). Returns io.EOF at a clean end of stream.
func (br *boxReader) next() (box []byte, typ string, err error) {
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

// rewriteTrexTrackID changes the track_ID inside a trex box (in place).
func rewriteTrexTrackID(b []byte, trexOff, trexSize int, newID uint32) error {
	if trexSize < 8+8 {
		return fmt.Errorf("trex at %d too small", trexOff)
	}
	binary.BigEndian.PutUint32(b[trexOff+8+4:], newID)
	return nil
}

// buildBox serializes a box with the given type and payload.
func buildBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// mergeInits merges the audio and video init parts (each ftyp + moov + emsg…)
// into one init for the interleaved stream: the audio ftyp, a single moov
// holding the audio trak (track_ID 1) and the renumbered video trak
// (track_ID 2, mvhd next_track_ID 3), then both emsg boxes. The video
// mvex/trex is renumbered to track_ID 2 and appended to the audio mvex —
// without a trex per track, strict demuxers (ffmpeg, PotPlayer) reject the
// fragments ("could not find corresponding trex"). Neither input is modified.
func mergeInits(audioInit, videoInit []byte) ([]byte, error) {
	aFtyp := findBox(audioInit, 0, len(audioInit), "ftyp")
	aMoov := findBox(audioInit, 0, len(audioInit), "moov")
	vMoov := findBox(videoInit, 0, len(videoInit), "moov")
	if aFtyp == nil || aMoov == nil {
		return nil, fmt.Errorf("audio init lacks ftyp/moov")
	}
	if findBox(audioInit, aMoov.off+8, aMoov.off+aMoov.size, "trak") == nil {
		return nil, fmt.Errorf("audio moov lacks trak")
	}
	if vMoov == nil {
		return nil, fmt.Errorf("video init lacks moov")
	}
	vTrak := findBox(videoInit, vMoov.off+8, vMoov.off+vMoov.size, "trak")
	if vTrak == nil {
		return nil, fmt.Errorf("video moov lacks trak")
	}
	vMvex := findBox(videoInit, vMoov.off+8, vMoov.off+vMoov.size, "mvex")
	if vMvex == nil {
		return nil, fmt.Errorf("video moov lacks mvex")
	}
	vTrex := findBox(videoInit, vMvex.off+8, vMvex.off+vMvex.size, "trex")
	if vTrex == nil {
		return nil, fmt.Errorf("video mvex lacks trex")
	}

	// Clone the buffers so the inputs stay untouched; offsets in the clones
	// match the originals.
	a := append([]byte(nil), audioInit...)
	v := append([]byte(nil), videoInit...)
	if err := rewriteTrakTrackID(v, vTrak.off, vTrak.size, 2); err != nil {
		return nil, fmt.Errorf("rewrite video trak id: %w", err)
	}
	if err := rewriteTrexTrackID(v, vTrex.off, vTrex.size, 2); err != nil {
		return nil, fmt.Errorf("rewrite video trex id: %w", err)
	}
	mvhd := findBox(a, aMoov.off+8, aMoov.off+aMoov.size, "mvhd")
	if mvhd == nil || mvhd.size < 8+100 {
		return nil, fmt.Errorf("audio mvhd not found or too small")
	}
	binary.BigEndian.PutUint32(a[mvhd.off+8+96:], 3) // next_track_ID
	aMvex := findBox(a, aMoov.off+8, aMoov.off+aMoov.size, "mvex")
	if aMvex == nil {
		return nil, fmt.Errorf("audio moov lacks mvex")
	}

	// Rebuild the merged moov: every audio moov box with the mvex payload
	// extended by the renumbered video trex, then the video trak appended.
	var moovContent []byte
	for _, bx := range walkBoxes(a, aMoov.off+8, aMoov.off+aMoov.size) {
		if bx.typ == "mvex" {
			mvexPayload := make([]byte, 0, bx.size-8+vTrex.size)
			mvexPayload = append(mvexPayload, a[bx.off+8:bx.off+bx.size]...)
			mvexPayload = append(mvexPayload, v[vTrex.off:vTrex.off+vTrex.size]...)
			moovContent = append(moovContent, buildBox("mvex", mvexPayload)...)
		} else {
			moovContent = append(moovContent, a[bx.off:bx.off+bx.size]...)
		}
	}
	moovContent = append(moovContent, v[vTrak.off:vTrak.off+vTrak.size]...)
	mergedMoov := buildBox("moov", moovContent)

	out := make([]byte, 0, len(a)+len(v))
	out = append(out, a[aFtyp.off:aFtyp.off+aFtyp.size]...)
	out = append(out, mergedMoov...)
	for _, src := range [][]byte{a, v} {
		for _, bx := range walkBoxes(src, 0, len(src)) {
			if bx.typ == "emsg" {
				out = append(out, src[bx.off:bx.off+bx.size]...)
			}
		}
	}
	return out, nil
}
