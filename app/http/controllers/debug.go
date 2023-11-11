package controllers

import (
	"encoding/base64"
	"github.com/gin-gonic/gin"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/websocket"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strings"
)

func Debug(r *gin.RouterGroup) {
	r.GET("/pprof/", func(ctx *gin.Context) { pprof.Index(ctx.Writer, ctx.Request) })
	r.GET("/pprof/:1", func(ctx *gin.Context) { pprof.Index(ctx.Writer, ctx.Request) })
	r.GET("/pprof/trace", func(ctx *gin.Context) { pprof.Trace(ctx.Writer, ctx.Request) })
	r.GET("/pprof/symbol", func(ctx *gin.Context) { pprof.Symbol(ctx.Writer, ctx.Request) })
	r.GET("/pprof/profile", func(ctx *gin.Context) { pprof.Profile(ctx.Writer, ctx.Request) })
	r.GET("/pprof/cmdline", func(ctx *gin.Context) { pprof.Cmdline(ctx.Writer, ctx.Request) })
	r.GET("/:method", func(c *gin.Context) {
		log := global.Log.WithField("function", "app.http.controllers.Debug")
		proxy := c.GetString("proxy")
		var proxyURL *url.URL
		var err error
		if proxy != "" {
			proxyURL, err = url.Parse(proxy)
			if err != nil {
				log.Errorf("parsing proxy error: %s\n", err.Error())
				c.String(400, "invalid proxy")
				return
			}
		}

		log.WithField("field", "url path").Debug(c.Request.URL.Path)
		log.WithField("field", "engine method").Debugf("%s\n", c.Param("method"))
		log.WithField("field", "query").Debug(c.Request.URL.RawQuery)
		if proxyURL != nil {
			log.WithField("field", "proxy").Debug(proxyURL.String())
		}
		headers := make(http.Header)
		if len(c.DefaultQuery("headers", "")) > 0 {
			hs, err := base64.StdEncoding.DecodeString(c.DefaultQuery("headers", ""))
			if err != nil {
				c.String(400, "decode headers error: %s", err.Error())
				return
			}
			log.WithField("field", "request headers from query").Debug(string(hs))
			gjson.GetBytes(hs, "data").ForEach(func(key, value gjson.Result) bool {
				if strings.ToLower(key.String()) != "user-agent" {
					headers.Set(key.String(), value.String())
				}
				return true
			})
		}
		log.WithField("field", "packed headers").Debug(headers)

		if len(c.DefaultQuery("url", "")) <= 0 {
			c.String(400, "no url found")
			return
		}
		fu, err := base64.StdEncoding.DecodeString(c.DefaultQuery("url", ""))
		if err != nil {
			c.String(400, "decode url error: %s", err.Error())
			return
		}

		switch strings.ToLower(c.Param("method")) {
		case "web", "http":
			f := httpweb.NewHTTPWebForwarder(proxyURL, false)

			err = f.Forward(c, headers, string(fu), 0)
			if err != nil {
				c.String(400, err.Error())
			}
		case "websocket", "ws":
			f := websocket.NewWebSocketForwarder(proxyURL, false)
			err = f.Start(c, string(fu))
			if err != nil {
				c.String(400, err.Error())
			}
		default:
			c.String(400, "unsupported forwarder")
			return
		}
	})
}
