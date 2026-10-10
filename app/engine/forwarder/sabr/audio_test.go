package sabr

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ---- fMP4 audio builders ----

func aBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// aMdhd builds a version-0 mdhd with the given timescale.
func aMdhd(timescale uint32) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint32(p[4:8], 0) // creation
	binary.BigEndian.PutUint32(p[12:16], timescale)
	binary.BigEndian.PutUint32(p[16:20], 0) // duration
	return aBox("mdhd", p)
}

// aEsds builds an esds whose descriptor tree carries a DecoderSpecificInfo.
func aEsds(csd []byte) []byte {
	dsi := append([]byte{0x05, byte(len(csd))}, csd...)
	dcd := append([]byte{0x04, byte(len(dsi) + 13)}, make([]byte, 13)...)
	dcd = append(dcd, dsi...)
	es := append([]byte{0x03, byte(len(dcd) + 3)}, 0x00, 0x00, 0x00)
	es = append(es, dcd...)
	body := append([]byte{0, 0, 0, 0}, es...) // version/flags
	return aBox("esds", body)
}

// aMp4a builds a minimal mp4a sample entry with channels and rate.
func aMp4a(channels uint16, rate uint32, csd []byte) []byte {
	p := make([]byte, 28)
	binary.BigEndian.PutUint16(p[16:18], channels)
	binary.BigEndian.PutUint32(p[24:28], rate<<16)
	return aBox("mp4a", append(p, aEsds(csd)...))
}

// aStsd wraps one sample entry.
func aStsd(entry []byte) []byte {
	body := make([]byte, 8) // version/flags + entry_count
	return aBox("stsd", append(body, entry...))
}

// aMoov builds an audio moov: trak > mdia > mdhd + minf > stbl > stsd.
func aMoov(timescale uint32, channels uint16, rate uint32, csd []byte) []byte {
	mdia := aBox("mdia", append(aMdhd(timescale), aBox("minf",
		aBox("stbl", aStsd(aMp4a(channels, rate, csd))))...))
	trak := aBox("trak", mdia)
	return aBox("moov", trak)
}

// aMoof builds a moof with tfdt (v1) and a trun sample table.
func aMoof(baseTicks uint64, sizes, durations []uint32) []byte {
	tfhd := make([]byte, 8)
	binary.BigEndian.PutUint32(tfhd[4:8], 1)
	tfdt := make([]byte, 12)
	tfdt[0] = 1
	binary.BigEndian.PutUint64(tfdt[4:12], baseTicks)
	flags := uint32(0x000100 | 0x000200) // duration + size per sample
	trunBody := make([]byte, 4)          // sample_count directly after version/flags
	binary.BigEndian.PutUint32(trunBody[0:4], uint32(len(sizes)))
	for i := range sizes {
		var entry [2]uint32
		entry[0] = durations[i]
		entry[1] = sizes[i]
		trunBody = append(trunBody,
			byte(entry[0]>>24), byte(entry[0]>>16), byte(entry[0]>>8), byte(entry[0]),
			byte(entry[1]>>24), byte(entry[1]>>16), byte(entry[1]>>8), byte(entry[1]))
	}
	trun := aBox("trun", append([]byte{0, byte(flags >> 16), byte(flags >> 8), byte(flags)}, trunBody...))
	traf := aBox("traf", append(append(aBox("tfhd", tfhd), aBox("tfdt", tfdt)...), trun...))
	return aBox("moof", traf)
}

func TestAACDemuxerFullRound(t *testing.T) {
	csd := []byte{0x12, 0x10} // a plausible ASC
	var doc []byte
	doc = append(doc, aBox("ftyp", []byte("ftypdash"))...)
	doc = append(doc, aMoov(44100, 2, 44100, csd)...)
	// One fragment: base 4410000 ticks (100s), two samples of 23.22ms each.
	f1 := []byte{0xAA, 0xBB}
	f2 := []byte{0xCC, 0xDD, 0xEE}
	doc = append(doc, aMoof(4410000, []uint32{2, 3}, []uint32{1024, 1024})...)
	doc = append(doc, aBox("mdat", append(append([]byte{}, f1...), f2...))...)
	// Second fragment: base 4412048 (the 1024-tick tick of sample 2 + gap).
	doc = append(doc, aMoof(4412048, []uint32{2}, []uint32{1024})...)
	doc = append(doc, aBox("mdat", f1)...)

	d := NewAACDemuxer()
	frames := d.Feed(doc)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	ts, rate, ch, gotCSD, ok := d.Meta()
	if !ok || ts != 44100 || rate != 44100 || ch != 2 {
		t.Errorf("meta = %d %d %d ok=%v, want 44100 44100 2 true", ts, rate, ch, ok)
	}
	if !bytes.Equal(gotCSD, csd) {
		t.Errorf("csd = %x, want %x", gotCSD, csd)
	}
	// 4410000 ticks / 44100 = 100000ms; each frame advances 1024 ticks
	// (~23.22ms).
	want := []int64{100000, 100023, 100046}
	for i, f := range frames {
		if f.TimecodeMs != want[i] {
			t.Errorf("frame %d tc = %d, want %d", i, f.TimecodeMs, want[i])
		}
	}
	if !bytes.Equal(frames[0].Data, f1) || !bytes.Equal(frames[1].Data, f2) {
		t.Errorf("frame data wrong: %x %x", frames[0].Data, frames[1].Data)
	}
}

func TestAACDemuxerIncremental(t *testing.T) {
	var doc []byte
	doc = append(doc, aMoov(44100, 2, 44100, []byte{0x12, 0x10})...)
	doc = append(doc, aMoof(0, []uint32{2}, []uint32{1024})...)
	doc = append(doc, aBox("mdat", []byte{0x11, 0x22})...)

	d := NewAACDemuxer()
	var frames []AACFrame
	for i := range doc {
		frames = append(frames, d.Feed(doc[i:i+1])...)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1 (incremental parsing broken)", len(frames))
	}
	if frames[0].TimecodeMs != 0 || !bytes.Equal(frames[0].Data, []byte{0x11, 0x22}) {
		t.Errorf("frame = %+v", frames[0])
	}
}

func TestAACDemuxerMissingDurationFallback(t *testing.T) {
	// trun without per-sample duration: the fixed AAC frame length applies.
	var doc []byte
	doc = append(doc, aMoov(44100, 2, 44100, []byte{0x12, 0x10})...)
	// Build a trun with only sizes (flags 0x200).
	tfhd := make([]byte, 8)
	tfdt := make([]byte, 12)
	tfdt[0] = 1
	trunBody := make([]byte, 4)
	binary.BigEndian.PutUint32(trunBody[0:4], 2)
	trunBody = append(trunBody, 0, 0, 0, 2, 0, 0, 0, 2)
	trun := aBox("trun", append([]byte{0, 0x00, 0x02, 0x00}, trunBody...))
	traf := aBox("traf", append(append(aBox("tfhd", tfhd), aBox("tfdt", tfdt)...), trun...))
	doc = append(doc, aBox("moof", traf)...)
	doc = append(doc, aBox("mdat", []byte{1, 2, 3, 4})...)

	d := NewAACDemuxer()
	frames := d.Feed(doc)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[1].TimecodeMs-frames[0].TimecodeMs != 23 {
		t.Errorf("fallback frame length = %dms, want 23", frames[1].TimecodeMs-frames[0].TimecodeMs)
	}
}

func TestParseTrunDataOffset(t *testing.T) {
	// YouTube's live audio trun: flags 0x0201 (data_offset + per-sample
	// size), 216 samples. The data_offset must not be eaten as a sample.
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 3)
	body = append(body, 0x01, 0x00, 0x00, 0x00) // data_offset (32-bit)
	body = append(body, 0, 0, 0, 37, 0, 0, 0, 41, 0, 0, 0, 37)
	var verFlags [4]byte
	binary.BigEndian.PutUint32(verFlags[:], 0x00000201)
	box := aBox("trun", append(verFlags[:], body...))
	s := parseTrun(box)
	if len(s) != 3 {
		t.Fatalf("samples = %d, want 3", len(s))
	}
	if s[0].size != 37 || s[1].size != 41 || s[2].size != 37 {
		t.Errorf("sizes = %v, want 37,41,37 (first entry must not read the data_offset)", s)
	}
}

func TestFindDescriptorCSDDirect(t *testing.T) {
	// A bare DecoderSpecificInfo descriptor.
	b := append([]byte{0x05, 0x03, 0x9A, 0x33, 0x0F}, 0x01)
	if csd := findDescriptorCSD(b); csd == nil || !bytes.Equal(csd, []byte{0x9A, 0x33, 0x0F}) {
		t.Errorf("csd = %v", csd)
	}
}

func TestFindDescriptorCSDRealESDS(t *testing.T) {
	// The descriptor layout from a live YouTube AAC esds box:
	// ES_Descriptor(3B header) > DecoderConfig(13B header) > DecSpecificInfo(ASC).
	desc := []byte{
		0x03, 0x19, // ES_Descriptor, len 25
		0x00, 0x01, 0x00, // ES_ID + flags
		0x04, 0x11, // DecoderConfigDescriptor, len 17 = 13 fixed + DSI
		0x40, 0x15, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 13 fixed bytes
		0x05, 0x02, // DecSpecificInfo, len 2
		0x12, 0x10, // AAC-LC ASC
		0x06, 0x01, 0x02, // SLConfigDescriptor
	}
	want := []byte{0x12, 0x10}
	csd := findDescriptorCSD(desc)
	if csd == nil || !bytes.Equal(csd, want) {
		t.Errorf("csd = %x, want %x", csd, want)
	}
}
