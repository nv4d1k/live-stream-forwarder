package flv

import (
	"bytes"
	"io"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// FLVStream wraps a *stream.Stream and prepends the cached FLV header before
// the media data from the pipe. HeaderCacheWriter strips headers from the
// pipe output, so every client gets exactly one header: cold clients wait
// for the writer to cache it, warm clients get the cached copy instantly.
// If the upstream never produces a valid FLV header (not FLV, or detection
// stalls), Read falls back to passing the inner stream through after
// HeaderWaitTimeout instead of hanging.
type FLVStream struct {
	inner     *stream.Stream
	cache     *HeaderCache
	key       string
	headerBuf io.Reader
	resolved  bool
}

func NewFLVStream(inner *stream.Stream, cache *HeaderCache, key string) *FLVStream {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.NewFLVStream")
	log.WithField("key", key).Debug("creating FLVStream")
	return &FLVStream{
		inner: inner,
		cache: cache,
		key:   key,
	}
}

func (f *FLVStream) Read(p []byte) (int, error) {
	// Resolve the header decision once, on the first Read, so the producer
	// goroutine (which may still be connecting upstream) has a chance to
	// cache the header first.
	if !f.resolved {
		f.resolved = true
		f.resolveHeader()
	}
	// If we have a cached header to prepend, send it first.
	if f.headerBuf != nil {
		n, err := f.headerBuf.Read(p)
		if err == io.EOF {
			f.headerBuf = nil
			return f.inner.Read(p)
		}
		return n, err
	}
	return f.inner.Read(p)
}

// resolveHeader decides, once, whether a cached header is prepended: a
// ready entry with data is used as-is; a cold entry is awaited (bounded by
// HeaderWaitTimeout); an entry marked missing or a timeout falls back to
// the headerless inner stream.
func (f *FLVStream) resolveHeader() {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.resolveHeader")
	entry := f.cache.GetOrCreate(f.key)
	if !entry.IsReady() {
		if !entry.WaitTimeout(HeaderWaitTimeout) {
			log.WithField("key", f.key).Warnf("timeout waiting for FLV header after %s, streaming without header", HeaderWaitTimeout)
			return
		}
	}
	data := entry.Data()
	if len(data) == 0 {
		// Entry resolved without a header (upstream is not valid FLV).
		log.WithField("key", f.key).Debug("no FLV header available, passing stream through")
		return
	}
	f.headerBuf = bytes.NewReader(data)
	log.WithField("key", f.key).WithField("size", len(data)).Debug("prepending cached FLV header")
}

func (f *FLVStream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.Close")
	log.WithField("key", f.key).Debug("closing FLVStream")
	return f.inner.Close()
}
