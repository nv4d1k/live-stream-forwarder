package websocket

import (
	"io"

	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
)

type Foreground interface {
	Start(*gin.Context, string) error
}

type Background interface {
	Start() error
	io.ReadCloser
}

// StreamResult wraps a *stream.Stream to satisfy io.ReadCloser.
// It is used when the websocket forwarder is in pipe mode with retry support.
type StreamResult struct {
	*stream.Stream
}
