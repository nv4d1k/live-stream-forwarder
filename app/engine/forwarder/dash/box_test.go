package dash

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ---- minimal ISO-BMFF builders used across the dash package tests ----

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

// mkTfdtV0 builds a version-0 tfdt (4-byte baseMediaDecodeTime).
func mkTfdtV0(tfdt uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[4:8], tfdt)
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

// mkInitPart builds an init fragment: ftyp + moov(+emsg).
func mkInitPart(trackID uint32, withEmsg bool) []byte {
	out := append([]byte{}, mkFtyp()...)
	out = append(out, mkMoov(trackID+1, mkTrak(trackID))...)
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

func TestWalkBoxes(t *testing.T) {
	b := append(append([]byte{}, mkFtyp()...), mkMoof(1, 1000)...)
	boxes := walkBoxes(b, 0, len(b))
	if len(boxes) != 2 {
		t.Fatalf("expected 2 boxes, got %d", len(boxes))
	}
	if boxes[0].typ != "ftyp" || boxes[1].typ != "moof" {
		t.Errorf("box types = %q,%q want ftyp,moof", boxes[0].typ, boxes[1].typ)
	}
	if boxes[0].off != 0 || boxes[1].off != boxes[0].size {
		t.Errorf("offsets wrong: %d,%d", boxes[0].off, boxes[1].off)
	}
}

func TestFindBox(t *testing.T) {
	b := mkMoov(3, mkTrak(1))
	moov := findBox(b, 0, len(b), "moov")
	if moov == nil {
		t.Fatal("moov not found")
	}
	if findBox(b, 0, len(b), "ftyp") != nil {
		t.Error("ftyp should not be found")
	}
}

func TestFirstMoofOffset(t *testing.T) {
	b := mkBatch(1, []uint64{1000, 2000})
	ftypLen := len(mkFtyp())
	moovLen := len(mkMoov(2, mkTrak(1)))
	emsgLen := len(mkEmsg())
	want := ftypLen + moovLen + emsgLen
	if got := firstMoofOffset(b); got != want {
		t.Errorf("firstMoofOffset = %d, want %d", got, want)
	}
	if got := firstMoofOffset(mkFtyp()); got != -1 {
		t.Errorf("firstMoofOffset without moof = %d, want -1", got)
	}
}

// findNested walks a box path (e.g. "moof","traf","tfhd") and returns the
// deepest box, or nil.
func findNested(b []byte, off, end int, path ...string) *boxInfo {
	cur := findBox(b, off, end, path[0])
	for _, p := range path[1:] {
		if cur == nil {
			return nil
		}
		cur = findBox(b, cur.off+8, cur.off+cur.size, p)
	}
	return cur
}

func TestRewriteTrakTrackID(t *testing.T) {
	trak := mkTrak(1)
	if err := rewriteTrakTrackID(trak, 0, len(trak), 2); err != nil {
		t.Fatalf("rewriteTrakTrackID error: %v", err)
	}
	tkhd := findNested(trak, 0, len(trak), "trak", "tkhd")
	if tkhd == nil {
		t.Fatal("tkhd not found inside trak")
	}
	got := binary.BigEndian.Uint32(trak[tkhd.off+8+12 : tkhd.off+8+16])
	if got != 2 {
		t.Errorf("track_ID after rewrite = %d, want 2", got)
	}
}

func TestRewriteMoofTrackIDs(t *testing.T) {
	moof := mkMoof(1, 1000)
	n := rewriteMoofTrackIDs(moof, 0, len(moof), 1, 2)
	if n != 1 {
		t.Fatalf("rewritten tfhd count = %d, want 1", n)
	}
	tfhd := findNested(moof, 0, len(moof), "moof", "traf", "tfhd")
	if tfhd == nil {
		t.Fatal("tfhd not found inside moof/traf")
	}
	got := binary.BigEndian.Uint32(moof[tfhd.off+8+4 : tfhd.off+8+8])
	if got != 2 {
		t.Errorf("tfhd track_ID = %d, want 2", got)
	}
	// No match for a different source ID.
	if n := rewriteMoofTrackIDs(moof, 0, len(moof), 9, 2); n != 0 {
		t.Errorf("rewrite from 9 should match nothing, got %d", n)
	}
}

func TestMoofTfdt(t *testing.T) {
	moof := mkMoof(1, 0xBEEF)
	got, ok := moofTfdt(moof, 0, len(moof))
	if !ok || got != 0xBEEF {
		t.Errorf("moofTfdt = %d,%v want 0xBEEF,true", got, ok)
	}
	// Version 0 (4-byte) tfdt.
	traf := mkBox("traf", append(append([]byte{}, mkTfhd(1)...), mkTfdtV0(777)...))
	moofV0 := mkBox("moof", append(append([]byte{}, mkMfhd(1)...), traf...))
	got, ok = moofTfdt(moofV0, 0, len(moofV0))
	if !ok || got != 777 {
		t.Errorf("moofTfdt v0 = %d,%v want 777,true", got, ok)
	}
	// No tfdt at all.
	bare := mkBox("moof", mkMfhd(1))
	if _, ok := moofTfdt(bare, 0, len(bare)); ok {
		t.Error("moofTfdt should report false without tfdt")
	}
}

func TestMergeInits(t *testing.T) {
	audioInit := mkInitPart(1, true) // ftyp + moov(mvhd next=2, trak id=1) + emsg
	videoInit := mkInitPart(1, true) // same shape for the video track

	merged, err := mergeInits(audioInit, videoInit)
	if err != nil {
		t.Fatalf("mergeInits error: %v", err)
	}

	boxes := walkBoxes(merged, 0, len(merged))
	var types []string
	for _, b := range boxes {
		types = append(types, b.typ)
	}
	// expect: ftyp, moov, emsg, emsg
	if len(types) != 4 || types[0] != "ftyp" || types[1] != "moov" || types[2] != "emsg" || types[3] != "emsg" {
		t.Fatalf("merged box types = %v, want [ftyp moov emsg emsg]", types)
	}

	// The merged moov must contain both traks: audio id=1, video id=2.
	moov := findBox(merged, 0, len(merged), "moov")
	var traks []boxInfo
	for _, b := range walkBoxes(merged, moov.off+8, moov.off+moov.size) {
		if b.typ == "trak" {
			traks = append(traks, b)
		}
	}
	if len(traks) != 2 {
		t.Fatalf("merged moov has %d traks, want 2", len(traks))
	}
	for i, want := range []uint32{1, 2} {
		tkhd := findBox(merged, traks[i].off+8, traks[i].off+traks[i].size, "tkhd")
		got := binary.BigEndian.Uint32(merged[tkhd.off+8+12 : tkhd.off+8+16])
		if got != want {
			t.Errorf("trak[%d] track_ID = %d, want %d", i, got, want)
		}
	}
	// mvhd next_track_ID must be 3.
	mvhd := findBox(merged, moov.off+8, moov.off+moov.size, "mvhd")
	if got := binary.BigEndian.Uint32(merged[mvhd.off+8+96 : mvhd.off+8+100]); got != 3 {
		t.Errorf("next_track_ID = %d, want 3", got)
	}
}

func TestMergeInits_MissingBoxes(t *testing.T) {
	if _, err := mergeInits(mkFtyp(), mkInitPart(1, false)); err == nil {
		t.Error("mergeInits should fail when audio init has no moov")
	}
	if _, err := mergeInits(mkInitPart(1, false), mkFtyp()); err == nil {
		t.Error("mergeInits should fail when video init has no moov")
	}
	// audio moov without a trak
	bare := append([]byte{}, mkFtyp()...)
	bare = append(bare, mkMoov(2)...)
	if _, err := mergeInits(bare, mkInitPart(1, false)); err == nil {
		t.Error("mergeInits should fail when audio moov has no trak")
	}
}
