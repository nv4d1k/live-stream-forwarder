package dash

// Minimal ISO-BMFF builders used by the dash package tests. The production
// implementations live in the bmff package; these helpers build the same
// box shapes for fixtures.

import (
	"bytes"
	"encoding/binary"
)

func mkBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func mkFtyp() []byte {
	return mkBox("ftyp", append([]byte("isom"), make([]byte, 8)...))
}

// mkMvhd builds a version-0 mvhd; nextTrackID sits at payload offset 96.
func mkMvhd(nextTrackID uint32) []byte {
	p := make([]byte, 100)
	binary.BigEndian.PutUint32(p[96:100], nextTrackID)
	return mkBox("mvhd", p)
}

// mkTkhd builds a version-0 tkhd; trackID sits at payload offset 12.
func mkTkhd(trackID uint32) []byte {
	p := make([]byte, 92)
	binary.BigEndian.PutUint32(p[12:16], trackID)
	return mkBox("tkhd", p)
}

func mkTrak(trackID uint32) []byte {
	return mkBox("trak", mkTkhd(trackID))
}

func mkMoov(nextTrackID uint32, traks ...[]byte) []byte {
	payload := append([]byte{}, mkMvhd(nextTrackID)...)
	for _, t := range traks {
		payload = append(payload, t...)
	}
	return mkBox("moov", payload)
}

// mkTrex: version/flags (4B) + trackID (4B) + default fields.
func mkTrex(trackID uint32) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint32(p[4:8], trackID)
	return mkBox("trex", p)
}

func mkMvex(trexes ...[]byte) []byte {
	var payload []byte
	for _, t := range trexes {
		payload = append(payload, t...)
	}
	return mkBox("mvex", payload)
}

func mkEmsg() []byte {
	return mkBox("emsg", []byte("e"))
}

// mkTfhd: version/flags (4B) + trackID (4B).
func mkTfhd(trackID uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[4:8], trackID)
	return mkBox("tfhd", p)
}

// mkTfdt builds a version-1 tfdt (8-byte baseMediaDecodeTime).
func mkTfdt(tfdt uint64) []byte {
	p := make([]byte, 12)
	p[0] = 1
	binary.BigEndian.PutUint64(p[4:12], tfdt)
	return mkBox("tfdt", p)
}

func mkMfhd(seq uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[4:8], seq)
	return mkBox("mfhd", p)
}

func mkMoof(trackID uint32, tfdt uint64) []byte {
	traf := mkBox("traf", append(append([]byte{}, mkTfhd(trackID)...), mkTfdt(tfdt)...))
	return mkBox("moof", append(append([]byte{}, mkMfhd(1)...), traf...))
}

func mkMdat(n int) []byte {
	return mkBox("mdat", bytes.Repeat([]byte{0xAB}, n))
}

// mkInitPart builds an init fragment: ftyp + moov(mvhd+trak+mvex/trex)(+emsg).
func mkInitPart(trackID uint32, withEmsg bool) []byte {
	moovPayload := append([]byte{}, mkMvhd(trackID+1)...)
	moovPayload = append(moovPayload, mkTrak(trackID)...)
	moovPayload = append(moovPayload, mkMvex(mkTrex(trackID))...)
	out := append([]byte{}, mkFtyp()...)
	out = append(out, mkBox("moov", moovPayload)...)
	if withEmsg {
		out = append(out, mkEmsg()...)
	}
	return out
}

// mkBatch builds a full track response: init part + moof/mdat pairs.
func mkBatch(trackID uint32, tfdts []uint64) []byte {
	out := append([]byte{}, mkInitPart(trackID, true)...)
	for _, t := range tfdts {
		out = append(out, mkMoof(trackID, t)...)
		out = append(out, mkMdat(16)...)
	}
	return out
}
