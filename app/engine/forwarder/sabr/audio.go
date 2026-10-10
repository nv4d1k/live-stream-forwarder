package sabr

import (
	"encoding/binary"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/bmff"
)

// AAC extraction from YouTube's live audio fMP4 stream (itag 140): each
// round's document is ftyp + moov + (emsg) + (moof + mdat)*. Every moof's
// trun carries the per-sample table (size + duration) and its tfdt the
// fragment's base time in track ticks (44100 on live AAC); slicing the
// following mdat along that table yields raw AAC frames — exactly what an
// MKV A_AAC track wants — with per-frame millisecond timecodes. The
// AudioSpecificConfig (CSD) is lifted from the moov's esds for the track's
// CodecPrivate.

// AACFrame is one raw AAC access unit with its absolute track-time
// millisecond timecode.
type AACFrame struct {
	TimecodeMs int64
	Data       []byte
}

// trunSample is one entry of a trun sample table.
type trunSample struct {
	size     uint32
	duration uint32
}

// AACDemuxer incrementally demuxes one round's audio document.
type AACDemuxer struct {
	pending []byte

	// moov metadata
	timescale  uint32 // mdhd timescale; 44100 on live AAC
	sampleRate uint32 // mp4a samplerate
	channels   uint16 // mp4a channel count
	csd        []byte // AudioSpecificConfig from esds
	initDone   bool

	// current fragment state: set by moof, consumed by the following mdat
	baseTicks uint64
	samples   []trunSample
}

// NewAACDemuxer creates a demuxer for one round's audio document.
func NewAACDemuxer() *AACDemuxer {
	return &AACDemuxer{timescale: 44100}
}

// Meta returns the moov metadata: track timescale, sample rate, channel
// count and the AudioSpecificConfig bytes. ok is false before the moov has
// been seen.
func (d *AACDemuxer) Meta() (timescale, sampleRate uint32, channels uint16, csd []byte, ok bool) {
	return d.timescale, d.sampleRate, d.channels, d.csd, d.initDone
}

// Feed appends document bytes and returns the AAC frames completed by this
// chunk.
func (d *AACDemuxer) Feed(b []byte) []AACFrame {
	d.pending = append(d.pending, b...)
	var frames []AACFrame
	for {
		boxes, consumed := bmff.WalkBoxesPartial(d.pending)
		if consumed == 0 {
			break
		}
		for _, bx := range boxes {
			raw := d.pending[bx.Off : bx.Off+bx.Size]
			switch bx.Typ {
			case "moov":
				d.parseMoov(raw)
			case "moof":
				d.parseMoof(raw)
			case "mdat":
				frames = append(frames, d.sliceMdat(raw[8:])...)
			default:
				// ftyp / emsg / free: skipped.
			}
		}
		d.pending = d.pending[consumed:]
	}
	return frames
}

// sliceMdat cuts one mdat payload into AAC frames along the current
// fragment's sample table. A sample without an explicit duration falls
// back to the fixed AAC frame length (1024 samples).
func (d *AACDemuxer) sliceMdat(body []byte) []AACFrame {
	if len(d.samples) == 0 {
		return nil
	}
	defDuration := uint32(0)
	if d.sampleRate > 0 {
		defDuration = uint32(uint64(1024) * uint64(d.timescale) / uint64(d.sampleRate))
	}
	var frames []AACFrame
	pos := 0
	ticks := d.baseTicks
	for _, s := range d.samples {
		if s.size == 0 || pos+int(s.size) > len(body) {
			// Truncated or empty sample: drop the rest of the fragment;
			// the next round's overlap re-fetches it.
			d.samples = nil
			return frames
		}
		frame := make([]byte, s.size)
		copy(frame, body[pos:pos+int(s.size)])
		pos += int(s.size)
		frames = append(frames, AACFrame{
			TimecodeMs: ticksToMs(ticks, d.timescale),
			Data:       frame,
		})
		dur := s.duration
		if dur == 0 {
			dur = defDuration
		}
		ticks += uint64(dur)
	}
	d.samples = nil
	return frames
}

// ticksToMs converts track ticks to milliseconds.
func ticksToMs(ticks uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	return int64(ticks * 1000 / uint64(timescale))
}

// parseMoov reads the first trak's mdhd timescale, mp4a parameters and
// esds CSD.
func (d *AACDemuxer) parseMoov(moov []byte) {
	for _, trak := range bmff.WalkBoxes(moov, 8, len(moov)) {
		if trak.Typ != "trak" {
			continue
		}
		d.parseTrak(moov[trak.Off : trak.Off+trak.Size])
		break
	}
	d.initDone = true
}

// parseTrak walks one trak's mdia for mdhd and minf.
func (d *AACDemuxer) parseTrak(trak []byte) {
	for _, mdia := range bmff.WalkBoxes(trak, 8, len(trak)) {
		if mdia.Typ != "mdia" {
			continue
		}
		body := trak[mdia.Off : mdia.Off+mdia.Size]
		for _, bx := range bmff.WalkBoxes(body, 8, len(body)) {
			switch bx.Typ {
			case "mdhd":
				d.parseMdhd(body[bx.Off : bx.Off+bx.Size])
			case "minf":
				d.parseMinf(body[bx.Off : bx.Off+bx.Size])
			}
		}
	}
}

// parseMdhd reads the track timescale (version 0 and 1 layouts).
func (d *AACDemuxer) parseMdhd(box []byte) {
	if len(box) < 8+16 {
		return
	}
	if box[8] == 1 {
		if len(box) >= 8+24 {
			d.timescale = binary.BigEndian.Uint32(box[8+20 : 8+24])
		}
		return
	}
	d.timescale = binary.BigEndian.Uint32(box[8+12 : 8+16])
}

// parseMinf digs stbl > stsd > mp4a for channel count, sample rate and the
// esds CSD.
func (d *AACDemuxer) parseMinf(minf []byte) {
	for _, stbl := range bmff.WalkBoxes(minf, 8, len(minf)) {
		if stbl.Typ != "stbl" {
			continue
		}
		stblBody := minf[stbl.Off : stbl.Off+stbl.Size]
		for _, stsd := range bmff.WalkBoxes(stblBody, 8, len(stblBody)) {
			if stsd.Typ != "stsd" {
				continue
			}
			stsdBody := stblBody[stsd.Off : stsd.Off+stsd.Size]
			// stsd: version/flags(4) + entry_count(4), then entries.
			for _, mp4a := range bmff.WalkBoxes(stsdBody, 8+8, len(stsdBody)) {
				if mp4a.Typ == "mp4a" {
					d.parseMP4A(stsdBody[mp4a.Off : mp4a.Off+mp4a.Size])
				}
			}
		}
	}
}

// parseMP4A reads the mp4a sample entry: channels and rate, plus the esds
// AudioSpecificConfig.
func (d *AACDemuxer) parseMP4A(box []byte) {
	// mp4a payload: version/flags(4) + reserved(6) + dataRef(2) +
	// channels(2)@16 + sampleSize(2)@18 + predef+reserved(4) + rate(4)@24.
	if len(box) >= 8+28 {
		d.channels = binary.BigEndian.Uint16(box[8+16 : 8+18])
		d.sampleRate = uint32(binary.BigEndian.Uint16(box[8+24 : 8+26]))
	}
	for _, esds := range bmff.WalkBoxes(box, 8+28, len(box)) {
		if esds.Typ == "esds" {
			if csd := parseESDSCSD(box[esds.Off : esds.Off+esds.Size]); csd != nil {
				d.csd = csd
			}
		}
	}
}

// parseESDSCSD extracts the DecoderSpecificInfo (descriptor tag 5) from an
// esds box payload.
func parseESDSCSD(esds []byte) []byte {
	if len(esds) < 8+4 {
		return nil
	}
	return findDescriptorCSD(esds[8+4:])
}

// findDescriptorCSD walks the MPEG-4 descriptor tree looking for the
// DecoderSpecificInfo (tag 5). Descriptor lengths use the 0x80-extension
// encoding. The two container descriptors carry fixed fields ahead of
// their children: ES_Descriptor (tag 3) opens with ES_ID (2 bytes) plus
// flags (1), DecoderConfigDescriptor (tag 4) with objectTypeIndication,
// streamType, bufferSizeDB, maxBitrate and avgBitrate (13 bytes total).
func findDescriptorCSD(b []byte) []byte {
	off := 0
	for off < len(b) {
		tag := b[off]
		off++
		length := 0
		for {
			if off >= len(b) {
				return nil
			}
			c := b[off]
			off++
			length = length<<7 | int(c&0x7F)
			if c&0x80 == 0 {
				break
			}
		}
		if off+length > len(b) {
			return nil
		}
		body := b[off : off+length]
		switch tag {
		case 0x05:
			out := make([]byte, len(body))
			copy(out, body)
			return out
		case 0x03:
			if len(body) > 3 {
				if csd := findDescriptorCSD(body[3:]); csd != nil {
					return csd
				}
			}
		case 0x04:
			if len(body) > 13 {
				if csd := findDescriptorCSD(body[13:]); csd != nil {
					return csd
				}
			}
		}
		off += length
	}
	return nil
}

// parseMoof reads the current fragment's tfdt and trun sample table.
func (d *AACDemuxer) parseMoof(moof []byte) {
	d.samples = nil
	d.baseTicks = 0
	for _, traf := range bmff.WalkBoxes(moof, 8, len(moof)) {
		if traf.Typ != "traf" {
			continue
		}
		trafBody := moof[traf.Off : traf.Off+traf.Size]
		for _, bx := range bmff.WalkBoxes(trafBody, 8, len(trafBody)) {
			switch bx.Typ {
			case "tfdt":
				d.baseTicks, _ = bmff.MoofTfdt(moof, 0, len(moof))
			case "trun":
				d.samples = parseTrun(trafBody[bx.Off : bx.Off+bx.Size])
			}
		}
	}
}

// parseTrun reads a trun box's per-sample table. The flags decide which
// header and per-sample fields are present; all fields are 32 bits wide.
// YouTube's live audio uses 0x0201: a data_offset in the header and one
// size per sample, with the duration defaulting to the AAC frame length.
func parseTrun(box []byte) []trunSample {
	if len(box) < 8+8 {
		return nil
	}
	flags := binary.BigEndian.Uint32(box[8:12]) & 0xFFFFFF
	count := binary.BigEndian.Uint32(box[12:16])
	hasDataOffset := flags&0x000001 != 0
	hasFirstFlags := flags&0x000004 != 0
	hasDuration := flags&0x000100 != 0
	hasSize := flags&0x000200 != 0
	hasFlags := flags&0x000400 != 0
	hasCTO := flags&0x000800 != 0
	entrySize := 0
	for _, has := range []bool{hasDuration, hasSize, hasFlags, hasCTO} {
		if has {
			entrySize += 4
		}
	}
	off := 16
	if hasDataOffset {
		off += 4
	}
	if hasFirstFlags {
		off += 4
	}
	samples := make([]trunSample, 0, min(count, 4096))
	for i := uint32(0); i < count; i++ {
		if off+entrySize > len(box) {
			break
		}
		var s trunSample
		p := off
		if hasDuration {
			s.duration = binary.BigEndian.Uint32(box[p : p+4])
			p += 4
		}
		if hasSize {
			s.size = binary.BigEndian.Uint32(box[p : p+4])
			p += 4
		}
		if hasFlags {
			p += 4
		}
		if hasCTO {
			p += 4
		}
		samples = append(samples, s)
		off += entrySize
	}
	return samples
}
