package sabr

import (
	"errors"
	"fmt"
	"io"
)

// UMP (Universal Media Protocol) framing. SABR conversations are streams of
// parts: <varint type><varint size><payload>, where the varint is YouTube's
// custom little-endian style encoding (NOT protobuf varint):
//
//	1 byte  (<128):  value = b1
//	2 bytes (128+):  value = (b1 & 0x3F) | (b2 << 6)
//	3 bytes (192+):  value = (b1 & 0x1F) | (b2 << 5) | (b3 << 13)
//	4 bytes (224+):  value = (b1 & 0x0F) | (b2 << 4) | (b3 << 12) | (b4 << 20)
//	5 bytes (240+):  value = little-endian uint32 of the 4 following bytes
//
// Requests wrap a serialized VideoPlaybackAbrRequest in a part of type 120;
// responses are sequences of the part types below.

// UMP part types appearing in SABR responses.
const (
	umpMediaHeader       = 20 // MediaHeader: per-track segment metadata
	umpMedia             = 21 // media bytes; payload[0] is the headerId it belongs to
	umpMediaEnd          = 22 // end of one track's segment in this response
	umpLiveMetadata      = 31 // SabrLiveMetadata: broadcast head position
	umpNextRequestPolicy = 35 // NextRequestPolicy: readahead targets + playback cookie
	umpSabrError         = 44 // SabrError
	umpSabrSeek          = 45 // SabrSeek
	umpStreamProtection  = 58 // StreamProtectionStatus: 2=pending, 3=reject
	umpEndOfTrack        = 62
)

// umpVarintSize returns the encoded length of v.
func umpVarintSize(v uint64) int {
	switch {
	case v < 0x80:
		return 1
	case v < 0x4000:
		return 2
	case v < 0x200000:
		return 3
	case v < 0x10000000:
		return 4
	default:
		return 5
	}
}

// appendUmpVarint appends the UMP varint encoding of v to b.
func appendUmpVarint(b []byte, v uint64) []byte {
	switch {
	case v < 0x80:
		return append(b, byte(v))
	case v < 0x4000:
		return append(b, byte(0x80|(v&0x3F)), byte(v>>6))
	case v < 0x200000:
		return append(b, byte(0xC0|(v&0x1F)), byte(v>>5), byte(v>>13))
	case v < 0x10000000:
		return append(b, byte(0xE0|(v&0x0F)), byte(v>>4), byte(v>>12), byte(v>>20))
	default:
		return append(b, 0xF0,
			byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
}

// umpVarint decodes one UMP varint from b at offset off. It returns the
// value and the offset just past it.
func umpVarint(b []byte, off int) (uint64, int, error) {
	if off >= len(b) {
		return 0, off, io.ErrUnexpectedEOF
	}
	first := b[off]
	switch {
	case first < 128:
		return uint64(first), off + 1, nil
	case first < 192:
		if off+2 > len(b) {
			return 0, off, io.ErrUnexpectedEOF
		}
		return uint64(first&0x3F) | uint64(b[off+1])<<6, off + 2, nil
	case first < 224:
		if off+3 > len(b) {
			return 0, off, io.ErrUnexpectedEOF
		}
		return uint64(first&0x1F) | uint64(b[off+1])<<5 | uint64(b[off+2])<<13, off + 3, nil
	case first < 240:
		if off+4 > len(b) {
			return 0, off, io.ErrUnexpectedEOF
		}
		return uint64(first&0x0F) | uint64(b[off+1])<<4 | uint64(b[off+2])<<12 | uint64(b[off+3])<<20, off + 4, nil
	default:
		if off+5 > len(b) {
			return 0, off, io.ErrUnexpectedEOF
		}
		p := off + 1
		return uint64(b[p]) | uint64(b[p+1])<<8 | uint64(b[p+2])<<16 | uint64(b[p+3])<<24, off + 5, nil
	}
}

// umpPart is one decoded UMP part.
type umpPart struct {
	Type    int
	Payload []byte
}

// errUMPFrame marks a malformed UMP stream (bad varint or implausible part
// size). Callers treat it like a corrupt upstream and re-extract.
var errUMPFrame = errors.New("sabr: malformed UMP frame")

// maxUMPPartSize bounds a single part's declared size; a response larger
// than this cannot be a legitimate media part and signals desynchronization.
const maxUMPPartSize = 64 << 20 // 64MB

// parseUMPParts extracts every complete UMP part from b. Incomplete trailing
// data is returned as rest so the caller can prepend it to the next chunk.
func parseUMPParts(b []byte) (parts []umpPart, rest []byte, err error) {
	off := 0
	for {
		typ, p, err := umpVarint(b, off)
		if err != nil {
			// Not enough bytes for the type (or a truncated varint): wait
			// for more data. A single-byte tail is always inconclusive.
			return parts, b[off:], nil
		}
		size, p2, err := umpVarint(b, p)
		if err != nil {
			return parts, b[off:], nil
		}
		if size > maxUMPPartSize {
			return nil, nil, fmt.Errorf("%w: part type %d declares %d bytes", errUMPFrame, typ, size)
		}
		if p2+int(size) > len(b) {
			// Header complete, payload still arriving.
			return parts, b[off:], nil
		}
		payload := b[p2 : p2+int(size)]
		parts = append(parts, umpPart{Type: int(typ), Payload: payload})
		off = p2 + int(size)
		if off == len(b) {
			return parts, nil, nil
		}
	}
}
