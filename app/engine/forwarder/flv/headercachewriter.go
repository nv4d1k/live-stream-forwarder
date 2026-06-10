package flv

import (
	"io"
	"sync"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

type detectState int

const (
	stateDetecting detectState = iota
	statePassthrough
)

// HeaderCacheWriter intercepts writes to detect and cache FLV header/config tags,
// then strips them from the output. Once all config tags are detected, subsequent
// writes pass through directly to the underlying writer.
type HeaderCacheWriter struct {
	next  io.Writer
	cache *HeaderCache
	key   string

	mu    sync.Mutex
	buf   []byte
	state detectState
}

func NewHeaderCacheWriter(next io.Writer, cache *HeaderCache, key string) *HeaderCacheWriter {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.NewHeaderCacheWriter")
	log.WithField("key", key).Debug("creating HeaderCacheWriter")
	return &HeaderCacheWriter{
		next:  next,
		cache: cache,
		key:   key,
		state: stateDetecting,
	}
}

func (w *HeaderCacheWriter) Write(p []byte) (int, error) {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.Write")
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == statePassthrough {
		return w.next.Write(p)
	}

	w.buf = append(w.buf, p...)

	offset := w.detectHeaderBoundary()
	switch {
	case offset < 0:
		// Not enough data yet, buffer and wait.
		return len(p), nil
	case offset == 0:
		// Not valid FLV or no header detected; passthrough everything.
		log.Debug("no FLV header detected, switching to passthrough mode")
		w.state = statePassthrough
		_, err := w.next.Write(w.buf)
		w.buf = nil
		return len(p), err
	default:
		// Header detected: cache it, write all buffered data to pipe (header is NOT stripped).
		log.WithField("headerSize", offset).Debug("FLV header detected and cached")
		entry := w.cache.GetOrCreate(w.key)
		entry.Set(w.buf[:offset])
		w.state = statePassthrough
		_, err := w.next.Write(w.buf)
		w.buf = nil
		return len(p), err
	}
}

// detectHeaderBoundary returns the byte offset where media data begins.
// Returns -1 if the boundary cannot be determined yet (incomplete data).
// Returns 0 if the data does not appear to be valid FLV.
func (w *HeaderCacheWriter) detectHeaderBoundary() int {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.detectHeaderBoundary")
	buf := w.buf

	// Minimum: FLV header (9) + PreviousTagSize0 (4) = 13
	if len(buf) < 13 {
		return -1
	}

	// Validate FLV signature.
	if buf[0] != 'F' || buf[1] != 'L' || buf[2] != 'V' {
		return 0
	}

	offset := 13 // past FLV header + PreviousTagSize0

	for offset < len(buf) {
		// Need at least 11 bytes for the tag header.
		if offset+11 > len(buf) {
			return -1
		}

		tagType := buf[offset]
		dataSize := int(buf[offset+1])<<16 | int(buf[offset+2])<<8 | int(buf[offset+3])
		totalTagSize := 11 + dataSize + 4 // tag header + data + PreviousTagSize

		if offset+totalTagSize > len(buf) {
			return -1
		}

		tagData := buf[offset+11 : offset+11+dataSize]

		if isFLVConfigTag(tagType, tagData) {
			offset += totalTagSize
			continue
		}

		// Media data tag found; this is the boundary.
		log.WithField("offset", offset).Debug("FLV header boundary found")
		return offset
	}

	// All tags so far are config tags; need more data.
	return -1
}

// isFLVConfigTag returns true if the tag is a configuration tag that should
// be cached as part of the FLV header (not media data).
func isFLVConfigTag(tagType byte, data []byte) bool {
	switch tagType {
	case 0x12: // Script Data (onMetaData etc.)
		return true
	case 0x08: // Audio
		if len(data) < 2 {
			return false
		}
		soundFormat := (data[0] >> 4) & 0x0F
		if soundFormat == 10 { // AAC
			return data[1] == 0 // AAC Sequence Header
		}
		return false
	case 0x09: // Video
		if len(data) < 2 {
			return false
		}
		frameType := (data[0] >> 4) & 0x0F
		codecID := data[0] & 0x0F
		if frameType == 1 { // Keyframe
			if codecID == 7 { // AVC/H.264
				return data[1] == 0 // AVC Sequence Header
			}
			if codecID == 12 { // HEVC/H.265
				return data[1] == 0 // HEVC Sequence Header
			}
		}
		return false
	default:
		return false
	}
}
