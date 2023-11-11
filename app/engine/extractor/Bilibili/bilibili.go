package BiliBili

import (
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Link struct {
	rid    string
	client *http.Client
}

func NewBiliBiliLink(rid string, proxy *url.URL) (bili *Link, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.Bilibili.NewBiliBiliLink")
	bili = new(Link)
	bili.rid = rid
	if proxy != nil {
		bili.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, true)}
	} else {
		bili.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, true)}
	}
	err = bili.getRoomStatus()
	if err != nil {
		return nil, fmt.Errorf("get room status error: %w", err)
	}
	log.WithField("field", "real room id").Debug(bili.rid)
	return bili, nil
}

func (l *Link) getRoomStatus() error {
	log := global.Log.WithField("function", "app.engine.extractor.Bilibili.getRoomStatus")
	resp, err := l.client.Get(fmt.Sprintf("https://api.live.bilibili.com/room/v1/Room/room_init?id=%s", l.rid))
	if err != nil {
		return fmt.Errorf("get room init info error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parse room init info body error: %w", err)
	}
	log.WithField("field", "room init data").Debug(string(body))
	if gjson.ParseBytes(body).Get("code").Int() != 0 {
		return errors.New("live streaming room not exist")
	}
	l.rid = gjson.ParseBytes(body).Get("data.room_id").String()
	if gjson.ParseBytes(body).Get("data.live_status").Int() != 1 {
		return errors.New("live streaming room is offline")
	}
	return nil
}

func (l *Link) GetLink() (*url.URL, error) {
	log := global.Log.WithField("function", "app.engine.extractor.Bilibili.GetLink")
	u, err := url.Parse("https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo")
	if err != nil {
		return nil, fmt.Errorf("parsing room play info url error: %w", err)
	}
	uq := u.Query()
	uq.Set("room_id", l.rid)
	uq.Set("protocol", "1")   // 0 = http_stream(flv), 1 = http_hls(m3u8)
	uq.Set("format", "0,1,2") // 0 = flv, 1 = ts, 2 = fmp4
	uq.Set("codec", "1")      // 0 = avc, 1 = hevc
	uq.Set("qn", "10000")
	uq.Set("platform", "h5")
	uq.Set("ptype", "8")
	u.RawQuery = uq.Encode()
	log.WithField("field", "rebuilt room play info url").Debug(u.String())
	resp, err := l.client.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("get room play info error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing room play info error: %w", err)
	}
	log.WithField("field", "room play info").Debug(string(body))
	streamsdata := gjson.ParseBytes(body).Get("data.playurl_info.playurl.stream")
	if !streamsdata.Exists() {
		return nil, errors.New("live streams not found")
	}
	stream := streamsdata.Get(`#(protocol_name=="http_hls").format.0.codec.0`)
	ur, err := url.Parse(stream.Get("url_info.0.host").String())
	if err != nil {
		return nil, fmt.Errorf("parsing host to url error: %w", err)
	}
	ur.Path = strings.ReplaceAll(stream.Get("base_url").String(), "?", "")
	ur.RawQuery = stream.Get("url_info.0.extra").String()

	return ur, nil
}
