package DouYin

import (
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Link struct {
	rid string

	cookies *http.Cookie
	client  *http.Client
}

func NewDouYinLink(rid string, proxy *url.URL) (douyin *Link, err error) {
	douyin = new(Link)
	douyin.rid = rid
	if proxy != nil {
		douyin.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		douyin.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	err = douyin.getCookies()
	if err != nil {
		return nil, fmt.Errorf("get cookies error: %w", err)
	}
	return douyin, nil
}

func (l *Link) getCookies() error {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.getCookies")
	req1, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/%s", l.rid), nil)
	if err != nil {
		return fmt.Errorf("making request for get __ac_nonce error: %w", err)
	}
	req1.Header.Set("Upgrade-Insecure-Requests", "1")
	resp1, err := l.client.Do(req1)
	if err != nil {
		return fmt.Errorf("get __ac_nonce error: %w", err)
	}
	defer resp1.Body.Close()
	log.WithField("field", "got __ac_nonce headers").Debug(resp1.Header)
	re := regexp.MustCompile(`(?i)__ac_nonce=([0-9a-f]*?);`)
	acNonce := re.FindStringSubmatch(resp1.Header.Get("Set-Cookie"))[1]
	if len(acNonce) <= 0 {
		return errors.New("__ac_nonce not found")
	}
	l.cookies = &http.Cookie{Name: "__ac_nonce", Value: acNonce}

	req2, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/%s", l.rid), nil)
	if err != nil {
		return fmt.Errorf("making request for get ttwid error: %w", err)
	}
	req2.Header.Set("Upgrade-Insecure-Requests", "1")
	req2.AddCookie(l.cookies)
	resp2, err := l.client.Do(req2)
	if err != nil {
		return fmt.Errorf("get ttwid error: %w", err)
	}
	defer resp2.Body.Close()
	log.WithField("field", "got ttwid headers").Debug(resp2.Header)
	re2 := regexp.MustCompile(`(?i)ttwid=(\S*);`)
	ttwid := re2.FindStringSubmatch(resp2.Header.Get("Set-Cookie"))[1]
	if len(ttwid) <= 0 {
		return fmt.Errorf("ttwid not found")
	}
	l.cookies = &http.Cookie{Name: "ttwid", Value: ttwid}
	return nil
}

func (l *Link) GetLink() (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.GetLink")
	req, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/webcast/room/web/enter/?aid=6383&app_name=douyin_web&live_id=1&device_platform=web&language=zh-CN&enter_from=web_live&cookie_enabled=true&screen_width=1728&screen_height=1117&browser_language=zh-CN&browser_platform=Windows&browser_name=Chrome&browser_version=117.0.0.0&web_rid=%s", l.rid), nil)
	if err != nil {
		return nil, fmt.Errorf("making request for get link error: %w", err)
	}
	req.AddCookie(l.cookies)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Host", "live.douyin.com")
	req.Header.Set("Connection", "keep-alive")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request for get link error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	log.WithField("field", "room json data").Debug(string(body))
	data := gjson.GetBytes(body, "data.data.0")
	if data.Get("status").Int() != 2 {
		return nil, fmt.Errorf("room status error: status %d", data.Get("status").Int())
	}
	stream := gjson.Parse(data.Get("stream_url.live_core_sdk_data.pull_data.stream_data").String())
	var (
		n int64 = -1
		u string
	)
	log.WithField("field", "stream data").Debug(stream.Raw)
	stream.Get("data").ForEach(func(key, value gjson.Result) bool {
		sdkParams := gjson.Parse(value.Get("main.sdk_params").String())
		if l.calcStreamWeight(sdkParams.Get("vbitrate").Int(), sdkParams.Get("resolution").String()) > n {
			n = l.calcStreamWeight(sdkParams.Get("vbitrate").Int(), sdkParams.Get("resolution").String())
			if value.Get("main.hls").Exists() {
				u = value.Get("main.hls").String()
				return true
			}
			u = value.Get("main.flv").String()
		}
		return true
	})
	return url.Parse(u)
}

func (l *Link) calcStreamWeight(vbitrate int64, resolution string) int64 {
	log := global.Log.WithField("function", "app.engine.extractor.DouYin.calcStreamWeight")
	if !strings.Contains(resolution, "x") {
		return -1
	}
	w, err := strconv.ParseInt(strings.Split(resolution, "x")[0], 10, 64)
	if err != nil {
		log.Errorf("convert height to int64 error: %s", err.Error())
	}
	h, err := strconv.ParseInt(strings.Split(resolution, "x")[1], 10, 64)
	if err != nil {
		log.Errorf("convert weight to int64 error: %s", err.Error())
	}
	return w * h * vbitrate
}
