package DouYin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

// QUALITIES is the preference order for stream quality tiers returned by the
// enter API. The picker takes the first tier that carries a URL.
var QUALITIES = []string{"FULL_HD1", "HD1", "SD1", "SD2"}

// enterAPIURL returns the room enter API endpoint for the given web_rid.
// The four browser_* params are mandatory: dropping any of them makes the
// API answer with an empty body.
func enterAPIURL(rid string) string {
	q := url.Values{}
	q.Set("aid", "6383")
	q.Set("app_name", "douyin_web")
	q.Set("live_id", "1")
	q.Set("device_platform", "web")
	q.Set("web_rid", rid)
	q.Set("browser_language", "zh-CN")
	q.Set("browser_platform", "MacIntel")
	q.Set("browser_name", "Chrome")
	q.Set("browser_version", "155.0.0.0")
	return "https://live.douyin.com/webcast/room/web/enter/?" + q.Encode()
}

func (l *Link) getCookies() error {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.getCookies")
	// ttwid is issued on the first page hit. When rate limiting kicks in the
	// page may only answer with __ac_nonce; carrying it into one retry usually
	// unlocks the ttwid response.
	for attempt := range 2 {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/%s", l.rid), nil)
		if err != nil {
			return fmt.Errorf("making request for ttwid error: %w", err)
		}
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		if l.cookies != nil {
			req.AddCookie(l.cookies)
		}
		log.WithField("attempt", attempt).Debugln("requesting room page for cookies")
		resp, err := l.client.Do(req)
		if err != nil {
			return fmt.Errorf("get ttwid error: %w", err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			log.WithError(err).Debugln("failed to drain response body")
		}

		var acNonce *http.Cookie
		for _, c := range resp.Cookies() {
			switch c.Name {
			case "ttwid":
				if c.Value != "" {
					log.Debugln("ttwid found")
					l.cookies = c
					return nil
				}
			case "__ac_nonce":
				if c.Value != "" {
					acNonce = c
				}
			}
		}
		if acNonce != nil && l.cookies == nil {
			log.Debugln("__ac_nonce found, retrying page request with it")
			l.cookies = acNonce
			continue
		}
		break
	}
	return errors.New("ttwid not found in response cookies")
}

func (l *Link) GetLink(format string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.GetLink")

	req, err := http.NewRequest("GET", enterAPIURL(l.rid), nil)
	if err != nil {
		return nil, fmt.Errorf("making request for get link error: %w", err)
	}
	if l.cookies != nil {
		req.AddCookie(l.cookies)
	}
	log.WithField("field", "sending enter api request").Debugf("%v\n", req)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request for get link error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	log.WithField("status", resp.StatusCode).Debugln("response of enter api request")
	data := gjson.ParseBytes(body)
	if err := enterAPIError(data); err != nil {
		log.WithError(err).Warnln("enter api reported an error")
		return nil, err
	}
	room := data.Get("data.data.0")
	u, err := pickStreamURL(room, format)
	if err != nil {
		log.WithError(err).Errorln("no stream url in enter api response")
		return nil, err
	}
	log.WithField("stream_url", u).Debugln("got stream url")
	return url.Parse(u)
}

// enterAPIError inspects an enter API response body and returns a descriptive
// error when the room cannot be watched (API error code set, no room in the
// list, or room without stream data). nil means the response is usable.
func enterAPIError(data gjson.Result) error {
	if code := data.Get("status_code").Int(); code != 0 {
		msg := data.Get("data.prompts").String()
		if msg == "" {
			msg = data.Get("data.message").String()
		}
		if msg == "" {
			return fmt.Errorf("enter api error code %d", code)
		}
		return fmt.Errorf("enter api error code %d: %s", code, msg)
	}
	rooms := data.Get("data.data")
	if !rooms.IsArray() || len(rooms.Array()) == 0 {
		return errors.New("no room found (room may be offline or not exist)")
	}
	if !rooms.Get("0.stream_url").IsObject() {
		return errors.New("room data carries no stream_url")
	}
	return nil
}

// pickStreamURL selects the pull URL for the given format from an enter API
// room object, always preferring the highest available quality tier.
func pickStreamURL(room gjson.Result, format string) (string, error) {
	field := "flv_pull_url"
	if format != "flv" {
		field = "hls_pull_url_map"
	}
	for _, quality := range QUALITIES {
		if u := room.Get(strings.Join([]string{"stream_url", field, quality}, ".")).String(); u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("no %s stream url available", field)
}

// expireAtFromURL derives the URL expiry time from its "expire" query
// parameter, an absolute Unix timestamp. It returns nil when the parameter is
// absent or unparsable.
func expireAtFromURL(u *url.URL) *time.Time {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.expireAtFromURL")
	raw := u.Query().Get("expire")
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		log.WithField("field", "expire").Debugf("ignoring unparsable expire value %q", raw)
		return nil
	}
	t := time.Unix(secs, 0)
	log.WithField("field", "expire at").Debugf("url expires at %s", t.Format(time.RFC3339))
	return &t
}
