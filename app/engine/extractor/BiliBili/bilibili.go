package BiliBili

import (
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Link struct {
	rid    string
	client *http.Client
}

func NewBiliBiliLink(rid string, proxy *url.URL) (bili *Link, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.BiliBili.NewBiliBiliLink")
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
	log := global.Log.WithField("function", "app.engine.extractor.BiliBili.getRoomStatus")
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

func (l *Link) getLinkByAPIv1() (*url.URL, error) {
	log := global.Log.WithField("function", "app.engine.extractor.BiliBili.getLinkByAPIv1")
	resp, err := l.client.Get(fmt.Sprintf("https://api.live.bilibili.com/xlive/web-room/v1/playUrl/playUrl?&https_url_req=1&qn=10000&platform=web&ptype=16&cid=%s", l.rid))
	if err != nil {
		return nil, fmt.Errorf("get api v1 response error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse api v1 response error: %w", err)
	}
	log.WithField("field", "api response").Debug(string(body))
	data := gjson.ParseBytes(body).Get("data")
	if data.Get("durl.0.url").Exists() && data.Get("durl.0.url").String() != "" {
		return url.Parse(data.Get("durl.0.url").String())
	}
	return nil, errors.New("no stream found")
}

func (l *Link) getLinkByAPIv2() (*url.URL, error) {
	log := global.Log.WithField("function", "app.engine.extractor.BiliBili.getLinkByAPIv2")
	u, err := url.Parse("https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo")
	if err != nil {
		return nil, fmt.Errorf("parsing room play info url error: %w", err)
	}
	uq := u.Query()
	uq.Set("room_id", l.rid)
	uq.Set("protocol", "0,1") // 0 = http_stream(flv), 1 = http_hls(m3u8)
	uq.Set("format", "0,1,2") // 0 = flv, 1 = ts, 2 = fmp4
	uq.Set("codec", "0,1")    // 0 = avc, 1 = hevc
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
	var (
		urls     []string
		parsestr string
	)
	parsestr = "#.format.#.codec.#|@flatten"
	if len(streamsdata.Get("#.format.#.codec.#(current_qn>=10000)|@flatten").Array()) > 0 {
		parsestr = "#.format.#.codec.#(current_qn>=10000)|@flatten"
	} else if len(streamsdata.Get("#.format.#.codec.#(current_qn>=250)|@flatten").Array()) > 0 {
		parsestr = "#.format.#.codec.#(current_qn>=250)|@flatten"
	}
	streamsdata.Get(parsestr).ForEach(func(ck, ci gjson.Result) bool {
		ci.Get("url_info").ForEach(func(uk, ui gjson.Result) bool {
			urls = append(urls, fmt.Sprintf("%s%s%s", ui.Get("host").String(), ci.Get("base_url").String(), ui.Get("extra").String()))
			return true
		})
		return true
	})
	log.WithField("field", "parsed url").Debug(strings.Join(urls, "\n"))
	if len(urls) <= 0 {
		return nil, errors.New("no streams found")
	}
	r := rand.New(rand.NewSource(time.Now().Unix()))
	return url.Parse(urls[r.Intn(len(urls)-1)])
}

func (l *Link) GetLink() (*url.URL, error) {
	/*log := global.Log.WithField("function", "app.engine.extractor.BiliBili.GetLink")
	u, err := l.getLinkByAPIv1()
	if err == nil {
		return u, nil
	}
	log.Errorf("trying get stream by api v1 error: %s\n", err.Error())*/
	u, err := l.getLinkByAPIv2()
	if err != nil {
		return nil, fmt.Errorf("trying get stream by api v2 error: %w", err)
	}
	return u, nil
}
