package Twitch

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.Twitch.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("twitch", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewTwitchLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

type Link struct {
	rid    string
	sig    string
	token  string
	client *http.Client
}

func NewTwitchLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Twitch.NewTwitchLink")
	tw := &Link{rid: rid}
	if proxy != nil {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	log.Debugf("creating Twitch extractor for room %s", rid)
	if err := tw.getSigToken(); err != nil {
		log.Errorf("failed to get sig/token for room %s: %v", rid, err)
		return nil, err
	}
	log.Infof("Twitch extractor created for room %s", rid)
	return tw, nil
}

func (l *Link) Extract(_ string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Twitch.Extract")
	u, err := l.GetLink(l.DefaultFormat())
	if err != nil {
		log.Errorf("failed to get link for room %s: %v", l.rid, err)
		return nil, err
	}
	log.Debugf("extracted stream URL for room %s", l.rid)
	return &extractor.Result{URL: u.String()}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"m3u8"}
}

func (l *Link) DefaultFormat() string {
	return "m3u8"
}

func (l *Link) GetLink(_ string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.Twitch.GetLink")
	params := url.Values{}
	params.Set("allow_source", "true")
	params.Set("allow_audio_only", "true")
	params.Set("allow_spectre", "true")
	params.Set("fast_bread", "true")
	params.Set("p", randP())
	params.Set("play_session_id", randHex(16))
	params.Set("player_backend", "mediaplayer")
	params.Set("playlist_include_framerate", "true")
	params.Set("reassignments_supported", "true")
	params.Set("sig", l.sig)
	params.Set("token", l.token)
	params.Set("cdm", "wv")
	params.Set("player_version", "1.30.0")

	usherURL := fmt.Sprintf("https://usher.ttvnw.net/api/channel/hls/%s.m3u8?%s", l.rid, params.Encode())
	log.Debugf("built usher URL for room %s", l.rid)
	return url.Parse(usherURL)
}
