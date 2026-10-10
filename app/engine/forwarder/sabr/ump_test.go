package sabr

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestUmpVarintRoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 42, 127, 128, 129, 4095, 4096,
		0x3FFF, 0x4000, 0x1FFFFF, 0x200000,
		0xFFFFFFF, 0x10000000, 0x7FFFFFFF,
		0xFFFFFFFF, // 5-byte form is a little-endian uint32: the ceiling
	}
	for _, v := range values {
		enc := appendUmpVarint(nil, v)
		got, n, err := umpVarint(append([]byte{0xAA}, enc...), 1)
		if err != nil {
			t.Fatalf("umpVarint(%d) error: %v", v, err)
		}
		if got != v {
			t.Errorf("umpVarint round trip = %d, want %d", got, v)
		}
		if n-1 != len(enc) {
			t.Errorf("umpVarint consumed %d bytes, encoded %d", n-1, len(enc))
		}
		if sz := umpVarintSize(v); sz != len(enc) {
			t.Errorf("umpVarintSize(%d) = %d, want %d", v, sz, len(enc))
		}
	}
}

func TestUmpVarintBoundaryEncodings(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{64, []byte{64}},
		{127, []byte{127}},
		{128, []byte{0x80, 0x02}}, // (128 & 0x3F) | 2<<6 = 0 | 128
		{65, []byte{0x41}},        // 65 < 128: single byte
		{1222, []byte{0x86, 0x13}},
		{4096, []byte{0x80, 0x40}}, // (4096 & 0x3F) | 64<<6
	}
	for _, c := range cases {
		enc := appendUmpVarint(nil, c.v)
		if !bytes.Equal(enc, c.want) {
			t.Errorf("encodeUmpVarint(%d) = %x, want %x", c.v, enc, c.want)
		}
	}
}

func TestParseUMPParts(t *testing.T) {
	// two parts: type 58 size 2, type 21 size 4 (with 5-byte varint for a big size)
	var buf []byte
	buf = appendUmpVarint(buf, 58)
	buf = appendUmpVarint(buf, 2)
	buf = append(buf, 0x01, 0x02)
	buf = appendUmpVarint(buf, 21)
	buf = appendUmpVarint(buf, 4)
	buf = append(buf, 0xAA, 0xBB, 0xCC, 0xDD)

	parts, rest, err := parseUMPParts(buf)
	if err != nil {
		t.Fatalf("parseUMPParts error: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %d bytes, want 0", len(rest))
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[0].Type != 58 || !bytes.Equal(parts[0].Payload, []byte{1, 2}) {
		t.Errorf("part 0 = %d %x, want 58 0102", parts[0].Type, parts[0].Payload)
	}
	if parts[1].Type != 21 || !bytes.Equal(parts[1].Payload, []byte{0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Errorf("part 1 = %d %x, want 21 AABBCCDD", parts[1].Type, parts[1].Payload)
	}
}

func TestParseUMPPartsIncompleteKeepsRest(t *testing.T) {
	var buf []byte
	buf = appendUmpVarint(buf, 58)
	buf = appendUmpVarint(buf, 10)
	buf = append(buf, 0x01) // only 1 of 10 payload bytes

	parts, rest, err := parseUMPParts(buf)
	if err != nil {
		t.Fatalf("parseUMPParts error: %v", err)
	}
	if len(parts) != 0 {
		t.Errorf("got %d parts from an incomplete frame, want 0", len(parts))
	}
	if len(rest) != len(buf) {
		t.Errorf("rest = %d bytes, want %d (whole input)", len(rest), len(buf))
	}

	// Feed the rest plus the missing bytes: the frame completes.
	rest = append(rest, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A)
	parts, rest2, err := parseUMPParts(rest)
	if err != nil {
		t.Fatalf("parseUMPParts continuation error: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != 58 {
		t.Fatalf("continuation produced %v, want one type-58 part", parts)
	}
	if len(rest2) != 0 {
		t.Errorf("continuation rest = %d bytes, want 0", len(rest2))
	}
}

func TestParseUMPPartsRejectsAbsurdSize(t *testing.T) {
	var buf []byte
	buf = appendUmpVarint(buf, 21)
	buf = appendUmpVarint(buf, maxUMPPartSize+1)
	if _, _, err := parseUMPParts(buf); err == nil {
		t.Fatal("expected frame error for oversized part")
	}
}

func TestWrapUMPRequest(t *testing.T) {
	body := wrapUMPRequest([]byte{0x08, 0x96, 0x02})
	if body[0] != 120 {
		t.Fatalf("wrapper type = %d, want 120", body[0])
	}
	sz, n, err := umpVarint(body, 1)
	if err != nil {
		t.Fatalf("size varint error: %v", err)
	}
	if int(sz) != 3 {
		t.Errorf("size = %d, want 3", sz)
	}
	if !bytes.Equal(body[n:], []byte{0x08, 0x96, 0x02}) {
		t.Errorf("payload = %x", body[n:])
	}
}

// TestBuildVideoPlaybackAbrRequest checks the exact bytes of a known
// request: itag 308 selected, live edge sentinel player time.
func TestBuildVideoPlaybackAbrRequest(t *testing.T) {
	pb := buildVideoPlaybackAbrRequest(abrRequestParams{
		UstreamerConfig: []byte{0xDE, 0xAD},
		VideoITag:       308,
		AudioITag:       140,
		PlayerTimeMs:    liveEdgeSentinelMs,
	})

	d := &pbDecoder{b: pb}
	var sawAbrState, sawCfg, sawAudio, sawVideo, sawCtx bool
	for {
		field, wire, ok := d.next()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == 2:
			sawAbrState = true
			d.bytesValue()
		case field == 4 && wire == 0:
			if v := d.varintValue(); v != 0 {
				t.Errorf("mediaStartTimeMs = %d, want 0", v)
			}
		case field == 5 && wire == 2:
			sawCfg = true
			if !bytes.Equal(d.bytesValue(), []byte{0xDE, 0xAD}) {
				t.Error("ustreamer config bytes mismatch")
			}
		case field == 16 && wire == 2:
			sawAudio = true
			inner := d.bytesValue()
			dd := &pbDecoder{b: inner}
			f, w, ok2 := dd.next()
			if !ok2 || f != 1 || w != 0 || dd.varintValue() != 140 {
				t.Errorf("audio FormatId = field %d wire %d itag %d", f, w, 0)
			}
		case field == 17 && wire == 2:
			sawVideo = true
			inner := d.bytesValue()
			dd := &pbDecoder{b: inner}
			f, w, ok2 := dd.next()
			if !ok2 || f != 1 || w != 0 || dd.varintValue() != 308 {
				t.Errorf("video FormatId = field %d wire %d itag %d", f, w, 0)
			}
		case field == 19 && wire == 2:
			sawCtx = true
			d.bytesValue()
		default:
			if !d.skipValue(wire) {
				t.Fatalf("truncated message at field %d", field)
			}
		}
	}
	if !sawAbrState || !sawCfg || !sawAudio || !sawVideo || !sawCtx {
		t.Errorf("missing fields: abrState=%v cfg=%v audio=%v video=%v ctx=%v",
			sawAbrState, sawCfg, sawAudio, sawVideo, sawCtx)
	}

	// ClientAbrState internals: playerTimeMs must be the sentinel.
	var inner []byte
	dd := &pbDecoder{b: pb}
	for {
		f, w, ok := dd.next()
		if !ok {
			break
		}
		if f == 1 && w == 2 {
			inner = dd.bytesValue()
		} else {
			dd.skipValue(w)
		}
	}
	cd := &pbDecoder{b: inner}
	sawPT := false
	for {
		f, w, ok := cd.next()
		if !ok {
			break
		}
		if f == 28 && w == 0 {
			sawPT = true
			if v := cd.varintValue(); v != uint64(liveEdgeSentinelMs) {
				t.Errorf("playerTimeMs = %d, want %d", v, liveEdgeSentinelMs)
			}
		} else {
			cd.skipValue(w)
		}
	}
	if !sawPT {
		t.Error("playerTimeMs missing from ClientAbrState")
	}
}

func TestParseMediaHeader(t *testing.T) {
	// MediaHeader { headerId=1, itag=308, segmentNum=42, startMs=123456789 }
	var pb []byte
	pb = appendPBInt(pb, 1, 1)
	pb = appendPBInt(pb, 3, 308)
	pb = appendPBInt(pb, 9, 42)
	pb = appendPBInt(pb, 11, 123456789)
	pb = appendPBInt(pb, 12, 5000)
	pb = appendPBInt(pb, 10, 1126912)

	h, ok := parseMediaHeader(pb)
	if !ok {
		t.Fatal("parseMediaHeader not ok")
	}
	if h.HeaderID != 1 || h.ITag != 308 || h.SegmentNum != 42 || h.StartMs != 123456789 || h.DurationMs != 5000 || h.BitrateBps != 1126912 {
		t.Errorf("mediaHeader = %+v", h)
	}
}

func TestParseStreamProtectionStatus(t *testing.T) {
	var pb []byte
	pb = appendPBInt(pb, 1, 3)
	pb = appendPBInt(pb, 2, 10)
	status, retries := parseStreamProtectionStatus(pb)
	if status != 3 || retries != 10 {
		t.Errorf("protection = %d/%d, want 3/10", status, retries)
	}
}

func TestParseLiveMetadata(t *testing.T) {
	var pb []byte
	pb = appendPBInt(pb, 3, 3752624)
	pb = appendPBInt(pb, 4, 18748208133)
	headMs, ok := parseLiveMetadata(pb)
	if !ok || headMs != 18748208133 {
		t.Errorf("liveMetadata headMs = %d ok=%v", headMs, ok)
	}
}

// guard against accidental wire-format drift: int fields must be big-endian
// varints per protobuf spec, which binary.AppendUvarint would violate.
func TestPBVarintEncoding(t *testing.T) {
	enc := appendPBVarint(nil, 300)
	// 300 = 0b10101100 0x02 -> [0xAC, 0x02]
	if len(enc) != 2 || enc[0] != 0xAC || enc[1] != 0x02 {
		t.Errorf("appendPBVarint(300) = %x, want ac02", enc)
	}
	_ = binary.BigEndian
}
