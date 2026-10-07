package BiliBili

import (
	"net/url"
	"strconv"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// expireAtFromURL derives the URL expiry time from its "expires" query
// parameter. BiliBili CDN URLs (flv and hls alike) carry an absolute
// Unix-seconds timestamp signed into the URL; once it passes, the CDN
// rejects the URL. Returns nil when no usable parameter exists, leaving
// the forwarder on passive (error-driven) refresh.
func expireAtFromURL(u *url.URL) *time.Time {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.expireAtFromURL")
	raw := u.Query().Get("expires")
	if raw == "" {
		return nil
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		log.WithField("field", "expires").Debugf("ignoring unparsable expires value %q", raw)
		return nil
	}
	t := time.Unix(secs, 0)
	log.WithField("field", "expire at").Debugf("url expires at %s", t.Format(time.RFC3339))
	return &t
}
