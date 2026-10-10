package sabr

import (
	"encoding/binary"
	"math"
)

// ANDROID innertube client identity for the SABR conversation. The player
// URL and ustreamer config come from an innertube request made with this
// same client (app/engine/extractor/YouTube/innertube.go); keep the two in
// sync.
const (
	androidClientVersion = "20.10.38"
	androidOSName        = "Android"
	androidOSVersion     = "15"
)

// Minimal protobuf wire-format helpers for the SABR conversation. Only the
// handful of messages YouTube's serverAbrStreamingUrl speaks are encoded or
// decoded: VideoPlaybackAbrRequest (out), MediaHeader / StreamProtectionStatus /
// SabrLiveMetadata (in). Field numbers come from the public googlevideo
// protocol descriptions.

// ---- encoding (standard protobuf wire format) ----

func appendPBTag(b []byte, field, wire int) []byte {
	return appendPBVarint(b, uint64(field<<3|wire))
}

func appendPBVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// appendPBInt appends a varint field (wire 0).
func appendPBInt(b []byte, field int, v uint64) []byte {
	b = appendPBTag(b, field, 0)
	return appendPBVarint(b, v)
}

// appendPBBytes appends a length-delimited field (wire 2).
func appendPBBytes(b []byte, field int, v []byte) []byte {
	b = appendPBTag(b, field, 2)
	b = appendPBVarint(b, uint64(len(v)))
	return append(b, v...)
}

// appendPBString appends a string field (wire 2).
func appendPBString(b []byte, field int, s string) []byte {
	return appendPBBytes(b, field, []byte(s))
}

// appendPBFloat appends a 32-bit float field (wire 5).
func appendPBFloat(b []byte, field int, f float32) []byte {
	b = appendPBTag(b, field, 5)
	var u [4]byte
	binary.LittleEndian.PutUint32(u[:], math.Float32bits(f))
	return append(b, u[:]...)
}

// appendPBMessage appends a nested message field (wire 2).
func appendPBMessage(b []byte, field int, m []byte) []byte {
	return appendPBBytes(b, field, m)
}

// abrRequestParams carries the state needed to serialize one
// VideoPlaybackAbrRequest.
type abrRequestParams struct {
	UstreamerConfig []byte
	VideoITag       int
	AudioITag       int
	PlayerTimeMs    int64
	ElapsedWallMs   int64
	StickyHeight    int
}

// liveEdgeSentinelMs is the playerTimeMs YouTube clients send to mean "start
// at the live head" (JavaScript's Number.MAX_SAFE_INTEGER).
const liveEdgeSentinelMs = int64(9007199254740991)

// formatIdMessage serializes a misc.FormatId { itag = 1 }.
func formatIdMessage(itag int) []byte {
	if itag <= 0 {
		return nil
	}
	return appendPBInt(nil, 1, uint64(itag))
}

// clientInfoMessage serializes a StreamerContext.ClientInfo for the
// anonymous ANDROID innertube client.
func clientInfoMessage() []byte {
	var b []byte
	b = appendPBString(b, 1, "en")                  // hl
	b = appendPBInt(b, 16, 3)                       // clientName: ANDROID
	b = appendPBString(b, 17, androidClientVersion) // clientVersion
	b = appendPBString(b, 18, androidOSName)        // osName
	b = appendPBString(b, 19, androidOSVersion)     // osVersion
	return b
}

// clientAbrStateMessage serializes the ClientAbrState submessage. The live
// edge sentinel in playerTimeMs asks the server to start at the head;
// stickyResolution hints the resolution we selected.
func clientAbrStateMessage(p abrRequestParams) []byte {
	var b []byte
	if p.StickyHeight > 0 {
		b = appendPBInt(b, 21, uint64(p.StickyHeight)) // stickyResolution
	}
	b = appendPBInt(b, 28, uint64(p.PlayerTimeMs))  // playerTimeMs
	b = appendPBInt(b, 29, uint64(p.ElapsedWallMs)) // timeSinceLastSeek
	b = appendPBInt(b, 34, 1)                       // visibility
	b = appendPBFloat(b, 35, 1.0)                   // playbackRate
	b = appendPBInt(b, 36, uint64(p.ElapsedWallMs)) // elapsedWallTimeMs
	b = appendPBInt(b, 39, uint64(p.ElapsedWallMs)) // timeSinceLastActionMs
	b = appendPBInt(b, 40, 0)                       // enabledTrackTypesBitfield: video+audio
	return b
}

// buildVideoPlaybackAbrRequest serializes a VideoPlaybackAbrRequest for the
// anonymous ANDROID client, selecting the given audio and video itags.
func buildVideoPlaybackAbrRequest(p abrRequestParams) []byte {
	var b []byte
	b = appendPBMessage(b, 1, clientAbrStateMessage(p)) // clientAbrState
	b = appendPBInt(b, 4, 0)                            // mediaStartTimeMs
	if len(p.UstreamerConfig) > 0 {
		b = appendPBBytes(b, 5, p.UstreamerConfig) // videoPlaybackUstreamerConfig
	}
	if aid := formatIdMessage(p.AudioITag); aid != nil {
		b = appendPBMessage(b, 16, aid) // selectedAudioFormatIds
	}
	if vid := formatIdMessage(p.VideoITag); vid != nil {
		b = appendPBMessage(b, 17, vid) // selectedVideoFormatIds
	}
	var sc []byte
	sc = appendPBMessage(sc, 1, clientInfoMessage())
	b = appendPBMessage(b, 19, sc) // streamerContext
	return b
}

// wrapUMPRequest frames a serialized protobuf message as the UMP request
// body: a single part of type 120 carrying the payload.
func wrapUMPRequest(pb []byte) []byte {
	out := make([]byte, 0, 1+umpVarintSize(uint64(len(pb)))+len(pb))
	out = append(out, 120)
	out = appendUmpVarint(out, uint64(len(pb)))
	return append(out, pb...)
}

// ---- decoding ----

// pbDecoder walks the fields of one protobuf message.
type pbDecoder struct {
	b   []byte
	off int
}

// next returns the next field number and wire type, or ok=false at the end.
func (d *pbDecoder) next() (field, wire int, ok bool) {
	if d.off >= len(d.b) {
		return 0, 0, false
	}
	var key uint64
	shift := 0
	for {
		if d.off >= len(d.b) {
			return 0, 0, false
		}
		c := d.b[d.off]
		d.off++
		key |= uint64(c&0x7F) << shift
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	return int(key >> 3), int(key & 7), true
}

// varintValue reads a varint value (after next returned wire 0).
func (d *pbDecoder) varintValue() uint64 {
	var v uint64
	shift := 0
	for d.off < len(d.b) {
		c := d.b[d.off]
		d.off++
		v |= uint64(c&0x7F) << shift
		shift += 7
		if c&0x80 == 0 {
			return v
		}
	}
	return v
}

// bytesValue reads a length-delimited value (after next returned wire 2).
func (d *pbDecoder) bytesValue() []byte {
	n := int(d.varintValue())
	if n < 0 || d.off+n > len(d.b) {
		return nil
	}
	v := d.b[d.off : d.off+n]
	d.off += n
	return v
}

// skipValue skips a value of the given wire type.
func (d *pbDecoder) skipValue(wire int) bool {
	switch wire {
	case 0:
		d.varintValue()
		return true
	case 1:
		if d.off+8 > len(d.b) {
			return false
		}
		d.off += 8
		return true
	case 5:
		if d.off+4 > len(d.b) {
			return false
		}
		d.off += 4
		return true
	case 2:
		n := int(d.varintValue())
		if n < 0 || d.off+n > len(d.b) {
			return false
		}
		d.off += n
		return true
	}
	return false
}

// mediaHeader mirrors the MediaHeader message fields the forwarder consumes.
type mediaHeader struct {
	HeaderID   uint32
	ITag       int32
	IsInit     bool
	SegmentNum int64
	StartMs    int64
	DurationMs int64
	BitrateBps int64
}

// parseMediaHeader decodes a MediaHeader payload.
func parseMediaHeader(pb []byte) (mediaHeader, bool) {
	var h mediaHeader
	d := &pbDecoder{b: pb}
	for {
		field, wire, ok := d.next()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == 0:
			h.HeaderID = uint32(d.varintValue())
		case field == 3 && wire == 0:
			h.ITag = int32(d.varintValue())
		case field == 8 && wire == 0:
			h.IsInit = d.varintValue() != 0
		case field == 9 && wire == 0:
			h.SegmentNum = int64(d.varintValue())
		case field == 10 && wire == 0:
			h.BitrateBps = int64(d.varintValue())
		case field == 11 && wire == 0:
			h.StartMs = int64(d.varintValue())
		case field == 12 && wire == 0:
			h.DurationMs = int64(d.varintValue())
		default:
			if !d.skipValue(wire) {
				return h, false
			}
		}
	}
	return h, h.ITag != 0
}

// parseStreamProtectionStatus decodes a StreamProtectionStatus payload:
// status 2 means attestation pending (streams still flow), 3 means rejected.
func parseStreamProtectionStatus(pb []byte) (status, maxRetries int32) {
	d := &pbDecoder{b: pb}
	for {
		field, wire, ok := d.next()
		if !ok {
			return
		}
		switch {
		case field == 1 && wire == 0:
			status = int32(d.varintValue())
		case field == 2 && wire == 0:
			maxRetries = int32(d.varintValue())
		default:
			if !d.skipValue(wire) {
				return
			}
		}
	}
}

// parseLiveMetadata decodes a SabrLiveMetadata payload; only the broadcast
// head (headMs) is consumed, for logging.
func parseLiveMetadata(pb []byte) (headMs int64, ok bool) {
	d := &pbDecoder{b: pb}
	for {
		field, wire, ok2 := d.next()
		if !ok2 {
			return headMs, ok
		}
		if field == 4 && wire == 0 {
			headMs = int64(d.varintValue())
			ok = true
		} else if !d.skipValue(wire) {
			return headMs, ok
		}
	}
}
