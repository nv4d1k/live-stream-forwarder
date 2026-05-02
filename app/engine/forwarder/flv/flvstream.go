package flv

import (
	"bytes"
	"io"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
)

// FLVStream wraps a *stream.Stream and prepends the cached FLV header
// before live stream data. New clients connecting mid-stream will receive
// the cached header first, then media data from the pipe.
type FLVStream struct {
	inner      *stream.Stream
	cache      *HeaderCache
	key        string
	headerBuf  *bytes.Reader
	headerSent bool
}

func NewFLVStream(inner *stream.Stream, cache *HeaderCache, key string) *FLVStream {
	return &FLVStream{
		inner: inner,
		cache: cache,
		key:   key,
	}
}

func (f *FLVStream) Read(p []byte) (int, error) {
	if !f.headerSent {
		if f.headerBuf == nil {
			entry := f.cache.GetOrCreate(f.key)
			entry.Wait()
			data := entry.Data()
			if data != nil {
				f.headerBuf = bytes.NewReader(data)
			} else {
				f.headerSent = true
				return f.inner.Read(p)
			}
		}

		n, err := f.headerBuf.Read(p)
		if err == io.EOF {
			f.headerBuf = nil
			f.headerSent = true
			return f.inner.Read(p)
		}
		return n, err
	}

	return f.inner.Read(p)
}

func (f *FLVStream) Close() error {
	return f.inner.Close()
}
