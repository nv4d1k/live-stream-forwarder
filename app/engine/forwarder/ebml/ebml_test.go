package ebml

import (
	"bytes"
	"testing"
)

// ---- tiny WebM builders mirroring YouTube's live document shape ----

// beBytes returns the minimal big-endian representation of v (>=1 byte).
func beBytes(v uint64) []byte {
	n := 1
	for v >= 1<<uint(8*n) {
		n++
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = byte(v >> uint(8*(n-1-i)))
	}
	return out
}

// elem builds ID + size vint + payload.
func elem(id uint64, payload []byte) []byte {
	out := AppendID(nil, id)
	out = AppendVint(out, uint64(len(payload)))
	return append(out, payload...)
}

// elemOpen builds ID + the 8-byte unknown-size vint (01 FF FF FF FF FF FF FF).
func elemOpen(id uint64) []byte {
	out := AppendID(nil, id)
	return append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
}

func simpleBlock(track uint64, relMs int16, keyframe bool, data []byte) []byte {
	var p []byte
	p = AppendVint(p, track)
	p = append(p, byte(uint16(relMs)>>8), byte(relMs))
	f := byte(0)
	if keyframe {
		f = 0x80
	}
	p = append(p, f)
	return append(p, data...)
}

func testWebMDoc(clusterTC int64, blockTf func(relMs int16) []byte) []byte {
	var doc []byte
	// EBML header: minimal
	doc = append(doc, elem(0x1A45DFA3, elem(0x4282, []byte("webm")))...)
	// Segment open
	doc = append(doc, elemOpen(idSegment)...)
	// Info: TimecodeScale 1e6
	info := elem(0x2AD7B1, []byte{0x0F, 0x42, 0x40}) // vint for 1000000
	doc = append(doc, elem(idInfo, info)...)
	// Tracks: one VP9 video track 2560x1440
	var entry []byte
	entry = append(entry, elem(idTrackNumber, []byte{0x01})...)
	entry = append(entry, elem(idTrackType, []byte{0x01})...)
	entry = append(entry, elem(idCodecID, []byte("V_VP9"))...)
	entry = append(entry, elem(idVideo, append(elem(idPixelWidth, []byte{0x0A, 0x00}), elem(idPixelHeight, []byte{0x05, 0xA0})...))...)
	doc = append(doc, elem(idTracks, elem(idTrackEntry, entry))...)
	// Cluster open with absolute timecode and blocks
	doc = append(doc, elemOpen(idCluster)...)
	doc = append(doc, elem(idClusterTimecode, beBytes(uint64(clusterTC)))...)
	doc = append(doc, elem(idSimpleBlock, simpleBlock(1, 0, true, []byte("key")))...)
	doc = append(doc, elem(idSimpleBlock, simpleBlock(1, 17, false, []byte("delta")))...)
	if blockTf != nil {
		doc = append(doc, blockTf(34)...)
	}
	return doc
}

func TestVintRoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 126, 127, 128, 16383, 16384,
		1000000, // YouTube's timecode scale
		1<<28 - 1, 1 << 28, 1<<49 - 1,
		18751018133, // live cluster timecode magnitude
	}
	for _, v := range values {
		enc := AppendVint(nil, v)
		got, next, ok := Vint(append([]byte{0xAA}, enc...), 1)
		if !ok {
			t.Fatalf("Vint(%d) not ok", v)
		}
		if got != v {
			t.Errorf("Vint round trip = %d, want %d (enc %x)", got, v, enc)
		}
		if next-1 != len(enc) {
			t.Errorf("Vint(%d) consumed %d bytes, encoded %d", v, next-1, len(enc))
		}
	}
}

func TestVintAllOnesReserved(t *testing.T) {
	// The all-ones value of every length means "unknown size"; encoding
	// must widen 127 (and friends) to the next length instead of emitting
	// a reserved pattern.
	for _, v := range []uint64{126, 127, 128, 16382, 16383, 16384, 2097150, 2097151} {
		enc := AppendVint(nil, v)
		if IsUnknownSize(v, len(enc)) {
			t.Errorf("AppendVint(%d) = %x collides with the unknown-size pattern", v, enc)
		}
		got, _, ok := Vint(enc, 0)
		if !ok || got != v {
			t.Errorf("round trip %d -> %x -> %d ok=%v", v, enc, got, ok)
		}
	}
}

func TestVintUnknownSizeDetection(t *testing.T) {
	// A 2-byte all-ones size vint means unknown.
	b := []byte{0x7F, 0xFF}
	val, next, ok := Vint(b, 0)
	if !ok || next != 2 {
		t.Fatalf("decode failed: %v %d %v", val, next, ok)
	}
	if !IsUnknownSize(val, 2) {
		t.Errorf("0xFEFF should be unknown size, got %d", val)
	}
	// A 8-byte unknown-size (YouTube's open Segment): 01 FF FF FF FF FF FF FF.
	b = append([]byte{0x01}, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	val, next, ok = Vint(b, 0)
	if !ok || !IsUnknownSize(val, next) {
		t.Errorf("8-byte unknown size: val=%d next=%d ok=%v", val, next, ok)
	}
}

func TestScanElemKnownAndUnknown(t *testing.T) {
	// Known: EBML header elem
	k := elem(0x1A45DFA3, []byte{1, 2, 3})
	e, next, needMore, err := ScanElem(k, 0)
	if err != nil || needMore {
		t.Fatalf("scan known: %v needMore=%v", err, needMore)
	}
	if e.ID != 0x1A45DFA3 || e.DataSize != 3 || next != len(k) {
		t.Errorf("elem = %+v next=%d", e, next)
	}
	// Unknown: open Segment
	u := elemOpen(idSegment)
	e, next, needMore, err = ScanElem(u, 0)
	if err != nil || needMore {
		t.Fatalf("scan unknown: %v", err)
	}
	if e.DataSize != -1 || next != 12 { // 4 ID + 8 size bytes
		t.Errorf("open elem = %+v next=%d, want size -1", e, next)
	}
	// Truncated payload: needMore
	trunc := k[:len(k)-2]
	if _, _, needMore, _ := ScanElem(trunc, 0); !needMore {
		t.Error("truncated payload must report needMore")
	}
}

func TestWebMFeedFullDocument(t *testing.T) {
	doc := testWebMDoc(18751018133, nil)
	d := NewWebM()
	frames, err := d.Feed(doc)
	if err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].TimecodeMs != 18751018133 || !frames[0].Keyframe {
		t.Errorf("frame0 = %+v", frames[0])
	}
	if frames[1].TimecodeMs != 18751018133+17 || frames[1].Keyframe {
		t.Errorf("frame1 = %+v", frames[1])
	}
	if !bytes.Equal(frames[0].Data, []byte("key")) || !bytes.Equal(frames[1].Data, []byte("delta")) {
		t.Errorf("frame data wrong: %q %q", frames[0].Data, frames[1].Data)
	}
	if got := d.CodecID(); got != "V_VP9" {
		t.Errorf("codec = %q", got)
	}
	if w, h := d.VideoSize(); w != 2560 || h != 1440 {
		t.Errorf("video size = %dx%d, want 2560x1440", w, h)
	}
}

func TestWebMFeedIncremental(t *testing.T) {
	doc := testWebMDoc(1000, nil)
	d := NewWebM()
	// Feed one byte at a time: frames appear only when complete.
	var frames []Frame
	var err error
	for i := range doc {
		fs, ferr := d.Feed(doc[i : i+1])
		frames = append(frames, fs...)
		err = ferr
	}
	if err != nil {
		t.Fatalf("incremental feed error: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (incremental parsing broken)", len(frames))
	}
	if frames[0].TimecodeMs != 1000 {
		t.Errorf("first frame tc = %d", frames[0].TimecodeMs)
	}
}

func TestWebMFeedMultipleFeedsAcrossBlocks(t *testing.T) {
	// Feed the doc split at an awkward boundary: inside the second block.
	doc := testWebMDoc(2000, nil)
	split := len(doc) - 3
	d := NewWebM()
	f1, err := d.Feed(doc[:split])
	if err != nil {
		t.Fatalf("feed1: %v", err)
	}
	f2, err := d.Feed(doc[split:])
	if err != nil {
		t.Fatalf("feed2: %v", err)
	}
	all := append(f1, f2...)
	if len(all) != 2 {
		t.Fatalf("frames = %d, want 2", len(all))
	}
	if all[1].TimecodeMs != 2017 {
		t.Errorf("frame1 tc = %d, want 2017", all[1].TimecodeMs)
	}
}

func TestWebMKnownSizeCluster(t *testing.T) {
	// A closed Cluster (size known) must parse identically.
	var doc []byte
	doc = append(doc, elem(0x1A45DFA3, elem(0x4282, []byte("webm")))...)
	doc = append(doc, elemOpen(idSegment)...)
	doc = append(doc, elem(idInfo, elem(0x2AD7B1, []byte{0x0F, 0x42, 0x40}))...)
	var entry []byte
	entry = append(entry, elem(idTrackNumber, []byte{0x01})...)
	entry = append(entry, elem(idCodecID, []byte("V_VP9"))...)
	doc = append(doc, elem(idTracks, elem(idTrackEntry, entry))...)

	var cluster []byte
	cluster = append(cluster, elem(idClusterTimecode, beBytes(5000))...)
	cluster = append(cluster, elem(idSimpleBlock, simpleBlock(1, 0, true, []byte("a")))...)
	doc = append(doc, elem(idCluster, cluster)...)
	doc = append(doc, elem(idClusterTimecode, beBytes(7000))...)
	// ^ deliberately at Segment level after a closed cluster: ignored

	d := NewWebM()
	frames, err := d.Feed(doc)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].TimecodeMs != 5000 {
		t.Errorf("tc = %d, want 5000", frames[0].TimecodeMs)
	}
}

func TestWebMBlockGroup(t *testing.T) {
	// Trailing frames sometimes arrive in BlockGroup > Block.
	var doc []byte
	doc = append(doc, elemOpen(idSegment)...)
	doc = append(doc, elem(idTracks, elem(idTrackEntry, append(elem(idTrackNumber, []byte{0x01}), elem(idCodecID, []byte("V_VP9"))...)))...)
	doc = append(doc, elemOpen(idCluster)...)
	doc = append(doc, elem(idClusterTimecode, beBytes(3000))...)
	block := simpleBlock(1, 10, false, []byte("bg"))
	doc = append(doc, elem(idBlockGroup, elem(idBlock, block))...)

	d := NewWebM()
	frames, err := d.Feed(doc)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 || frames[0].TimecodeMs != 3010 {
		t.Fatalf("BlockGroup frame = %+v, want tc 3010", frames)
	}
}

func TestAppendIDAndIDLen(t *testing.T) {
	cases := []struct {
		id   uint64
		want []byte
	}{
		{0xAE, []byte{0xAE}},
		{0xE7, []byte{0xE7}},
		{0xA3, []byte{0xA3}},
		{0x1F43B675, []byte{0x1F, 0x43, 0xB6, 0x75}},
		{0x1A45DFA3, []byte{0x1A, 0x45, 0xDF, 0xA3}},
		{0x18538067, []byte{0x18, 0x53, 0x80, 0x67}},
	}
	for _, c := range cases {
		got := AppendID(nil, c.id)
		if !bytes.Equal(got, c.want) {
			t.Errorf("AppendID(%#x) = %x, want %x", c.id, got, c.want)
		}
		if IDLen(c.id) != len(c.want) {
			t.Errorf("IDLen(%#x) = %d, want %d", c.id, IDLen(c.id), len(c.want))
		}
	}
}
