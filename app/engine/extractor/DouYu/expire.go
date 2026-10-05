package DouYu

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

// expireAtFromURL derives the URL expiry time from its query parameters.
// DouYu WebSocket URLs carry a relative "expire" (seconds); tx-family CDN URLs
// carry an absolute hex "txTime" Unix timestamp. When both are present the
// earlier one wins, so a refresh is never scheduled later than the real
// expiry. It returns nil when no usable expiry parameter exists.
func expireAtFromURL(u *url.URL) *time.Time {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.expireAtFromURL")
	var earliest *time.Time

	if raw := u.Query().Get("expire"); raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
			t := time.Now().Add(time.Duration(secs) * time.Second)
			earliest = &t
		} else {
			log.WithField("field", "expire").Debugf("ignoring unparsable expire value %q", raw)
		}
	}

	if raw := u.Query().Get("txTime"); raw != "" {
		if secs, err := strconv.ParseInt(raw, 16, 64); err == nil && secs > 0 {
			t := time.Unix(secs, 0)
			if earliest == nil || t.Before(*earliest) {
				earliest = &t
			}
		} else {
			log.WithField("field", "txTime").Debugf("ignoring unparsable txTime value %q", raw)
		}
	}

	if earliest != nil {
		log.WithField("field", "expire at").Debugf("url expires at %s", earliest.Format(time.RFC3339))
	}
	return earliest
}

// rateStreamError returns a human-readable reason when the getH5PlayV1
// response carries no usable stream (plain-text rejection such as "鉴权失败",
// API error code set, or empty rtmp_live meaning the room is not
// broadcasting). Empty string means OK.
func rateStreamError(data gjson.Result) string {
	if !data.IsObject() {
		if msg := strings.TrimSpace(data.String()); msg != "" {
			return msg
		}
		return "empty rate stream response"
	}
	if code := data.Get("error").Int(); code != 0 {
		if msg := data.Get("msg").String(); msg != "" {
			return msg
		}
		return fmt.Sprintf("error code %d", code)
	}
	if data.Get("data.rtmp_live").String() == "" {
		return "no stream available (room may be offline)"
	}
	return ""
}
