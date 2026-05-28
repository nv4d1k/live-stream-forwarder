package Kick

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

const defaultAPIBase = "https://kick.com"

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.Kick.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("kick", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewKickLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

type channelResponse struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	PlaybackURL string `json:"playback_url"`
	Livestream  *struct {
		ID           int64  `json:"id"`
		IsLive       bool   `json:"is_live"`
		SessionTitle string `json:"session_title"`
		ViewerCount  int    `json:"viewer_count"`
	} `json:"livestream"`
}

// Link implements extractor.Extractor for Kick.com live streams.
type Link struct {
	rid     string
	client  *http.Client
	apiBase string
}

// NewKickLink creates a Kick extractor for the given channel slug.
func NewKickLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Kick.NewKickLink")
	k := &Link{rid: rid, apiBase: defaultAPIBase}
	if proxy != nil {
		k.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		k.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	log.Debugf("creating Kick extractor for room %s", rid)
	log.Infof("Kick extractor created for room %s", rid)
	return k, nil
}

func (l *Link) Extract(_ string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Kick.Extract")

	ch, err := l.getChannel()
	if err != nil {
		log.Errorf("failed to get channel info for room %s: %v", l.rid, err)
		return nil, err
	}

	if ch.Livestream == nil || !ch.Livestream.IsLive {
		log.Warnf("channel %s is offline", l.rid)
		return nil, fmt.Errorf("channel %s is offline", l.rid)
	}

	if ch.PlaybackURL == "" {
		log.Warnf("channel %s has no playback URL", l.rid)
		return nil, fmt.Errorf("channel %s has no playback URL", l.rid)
	}

	headers := make(http.Header)
	headers.Set("Referer", "https://kick.com/")
	headers.Set("Origin", "https://kick.com")

	result := &extractor.Result{URL: ch.PlaybackURL, Headers: headers}

	if exp, err := parseJWTExp(ch.PlaybackURL); err == nil {
		result.ExpireAt = &exp
		log.Debugf("playback URL expires at %s for room %s", exp.Format(time.RFC3339), l.rid)
	} else {
		log.Debugf("could not parse JWT exp for room %s: %v", l.rid, err)
	}

	log.Debugf("extracted stream URL for room %s", l.rid)
	return result, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"m3u8"}
}

func (l *Link) DefaultFormat() string {
	return "m3u8"
}

func (l *Link) getChannel() (*channelResponse, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Kick.getChannel")

	apiURL := l.apiBase + "/api/v2/channels/" + url.PathEscape(l.rid)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://kick.com/"+l.rid)

	resp, err := l.client.Do(req)
	if err != nil {
		log.Errorf("API request failed for room %s: %v", l.rid, err)
		return nil, fmt.Errorf("request channel API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warnf("API returned status %d for room %s", resp.StatusCode, l.rid)
		return nil, fmt.Errorf("channel API returned status %d", resp.StatusCode)
	}

	var ch channelResponse
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		log.Errorf("failed to decode API response for room %s: %v", l.rid, err)
		return nil, fmt.Errorf("decode channel response: %w", err)
	}

	log.Debugf("got channel info for room %s (id=%d, is_live=%v)", l.rid, ch.ID, ch.Livestream != nil && ch.Livestream.IsLive)
	return &ch, nil
}

// parseJWTExp extracts the exp claim from the JWT token in the playback URL's
// query parameter. Returns the expiration time or an error if parsing fails.
func parseJWTExp(playbackURL string) (time.Time, error) {
	u, err := url.Parse(playbackURL)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse playback URL: %w", err)
	}
	tokenStr := u.Query().Get("token")
	if tokenStr == "" {
		return time.Time{}, fmt.Errorf("no token query parameter")
	}

	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	payload, err := base64urlDecode(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("unmarshal JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("JWT has no exp claim")
	}

	return time.Unix(claims.Exp, 0), nil
}

func base64urlDecode(s string) ([]byte, error) {
	// Add padding if needed.
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}
