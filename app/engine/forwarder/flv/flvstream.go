package flv

import (
	"bytes"
	"io"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// FLVStream wraps a *stream.Stream and prepends the cached FLV header
// before live stream data for mid-stream clients. If the cache has no
// header yet, data passes through directly from the inner stream
// (which already includes the FLV header from HeaderCacheWriter).
type FLVStream struct {
	inner     *stream.Stream
	cache     *HeaderCache
	key       string
	headerBuf io.Reader
}

func NewFLVStream(inner *stream.Stream, cache *HeaderCache, key string) *FLVStream {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.NewFLVStream")
	log.WithField("key", key).Debug("creating FLVStream")
	f := &FLVStream{
		inner: inner,
		cache: cache,
		key:   key,
	}
	// If a cached header already exists, prepare it for prepending.
	// This handles mid-stream clients that connect after the header
	// has already passed through the pipe.
	if entry := cache.GetOrCreate(key); entry.IsReady() {
		if data := entry.Data(); data != nil {
			f.headerBuf = bytes.NewReader(data)
		}
	}
	return f
}

func (f *FLVStream) Read(p []byte) (int, error) {
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

func (f *FLVStream) Close() error {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.Close")
	log.WithField("key", f.key).Debug("closing FLVStream")
	return f.inner.Close()
}
