package websocket

import (
	"io"

	"github.com/gin-gonic/gin"
)

type Foreground interface {
	Start(*gin.Context, string) error
}

type Background interface {
	Start() error
	io.ReadCloser
}
