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
	douyin.cookies = &http.Cookie{}
	err = douyin.getCookies()
	if err != nil {
		return nil, fmt.Errorf("get cookies error: %w", err)
	}
	return douyin, nil
}

func (l *Link) getCookies() error {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.getCookies")
	reAcNonce := regexp.MustCompile(`(?i)__ac_nonce=([0-9a-f]*?);`)
	reTtwid := regexp.MustCompile(`(?i)ttwid=(\S*);`)

	req, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/%s", l.rid), nil)
	if err != nil {
		return fmt.Errorf("making request for get __ac_nonce or ttwid error: %w", err)
	}
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.AddCookie(l.cookies)
	log.WithField("field", "sending requests").Debugf("%v\n", req)
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("get get __ac_nonce or ttwid error: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case reAcNonce.MatchString(resp.Header.Get("Set-Cookie")):
		acNonce := reAcNonce.FindStringSubmatch(resp.Header.Get("Set-Cookie"))[1]
		l.cookies = &http.Cookie{Name: "__ac_nonce", Value: acNonce}
		return l.getCookies()
	case reTtwid.MatchString(resp.Header.Get("Set-Cookie")):
		ttwid := reTtwid.FindStringSubmatch(resp.Header.Get("Set-Cookie"))[1]
		l.cookies = &http.Cookie{Name: "ttwid", Value: ttwid}
		return nil
	default:
		return errors.New("both __ac_nonce and ttwid are not found")
	}
}

func (l *Link) GetLink(format string) (*url.URL, error) {
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
	switch {
	case stream.Get("data.origin").Exists():
		if format == "flv" {
			u = stream.Get("data.origin.main.flv").String()
			log.WithField("stream_url", u).Debugln("get origin flv url")
			return url.Parse(u)
		}
		u = stream.Get("data.origin.main.hls").String()
		log.WithField("stream_url", u).Debugln("get origin hls url")
		return url.Parse(u)
	default:
		stream.Get("data").ForEach(func(key, value gjson.Result) bool {
			sdkParams := gjson.Parse(value.Get("main.sdk_params").String())
			if l.calcStreamWeight(sdkParams.Get("vbitrate").Int(), sdkParams.Get("resolution").String()) > n {
				n = l.calcStreamWeight(sdkParams.Get("vbitrate").Int(), sdkParams.Get("resolution").String())
				if format == "flv" {
					u = value.Get("main.flv").String()
					return true
				}
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
