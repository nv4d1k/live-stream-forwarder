package controllers

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static/cookie.html
var staticFS embed.FS

func CookieTool(c *gin.Context) {
	data, err := staticFS.ReadFile("static/cookie.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "tool page not found")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}
