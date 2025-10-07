package DouYin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/antchfx/htmlquery"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

var QIALITIES = []string{"origin", "hd", "sd", "ld", "md"}

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
		log.WithField("__ac_nonce", acNonce).Debugln("__ac_nonce found")
		l.cookies = &http.Cookie{Name: "__ac_nonce", Value: acNonce}
		/*
				return l.getCookies()
			case reTtwid.MatchString(resp.Header.Get("Set-Cookie")):
				ttwid := reTtwid.FindStringSubmatch(resp.Header.Get("Set-Cookie"))[1]
				l.cookies = &http.Cookie{Name: "ttwid", Value: ttwid}
		*/
	case reTtwid.MatchString(resp.Header.Get("Set-Cookie")):
		ttwid := reTtwid.FindStringSubmatch(resp.Header.Get("Set-Cookie"))[1]
		log.WithField("ttwid", ttwid).Debugln("ttwid found")
		l.cookies = &http.Cookie{Name: "ttwid", Value: ttwid}
	default:
		return errors.New("both __ac_nonce and ttwid are not found")
	}
	return nil
}

func (l *Link) GetLink(format string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.GetLink")

	req, err := http.NewRequest("GET", fmt.Sprintf("https://live.douyin.com/%s", l.rid), nil)
	if err != nil {
		return nil, fmt.Errorf("making request for get link error: %w", err)
	}
	req.AddCookie(l.cookies)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Host", "live.douyin.com")
	req.Header.Set("Connection", "keep-alive")
	log.WithField("field", "sending requests with cookies").Debugf("%v\n", req)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request for get link error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	log.WithField("headers", resp.Header).Debugln("response header of request with cookies")
	doc, err := htmlquery.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	var liveData string
	nodes := htmlquery.Find(doc, "//script")
	for _, node := range nodes {
		log.WithField("field", "script node").Debugln(htmlquery.InnerText(node))
		content, ok := l.extractJSON(htmlquery.InnerText(node))
		if ok {
			liveData = content
			break
		}
	}
	var stream gjson.Result
	for _, quality := range QIALITIES {
		if gjson.Get(liveData, fmt.Sprintf("data.%s", quality)).Exists() {
			stream = gjson.Get(liveData, fmt.Sprintf("data.%s", quality))
			break
		}
	}
	var (
		u string
	)
	log.WithField("field", "stream data").Debugln(stream.Raw)
	switch format {
	case "flv":
		u = stream.Get("main.flv").String()
		log.WithField("stream_url", u).Debugln("get origin flv url")
		return url.Parse(u)
	default:
		u = stream.Get("main.hls").String()
		log.WithField("stream_url", u).Debugln("get origin hls url")
		return url.Parse(u)
	}
}

func (l *Link) extractJSON(input string) (string, bool) {
	re := regexp.MustCompile(`self\.__pace_f\.push\(\[1,"(.*)"\]\)`)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 2 {
		return "", false
	}

	// Original escaped JSON
	escaped := matches[1]

	// Unescape to clean JSON
	var raw string
	if err := json.Unmarshal([]byte(`"`+escaped+`"`), &raw); err != nil {
		return "", false
	}

	// Verify it's valid JSON and check if the "common" field exists
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", false
	}

	// Go maps are unordered, so we can't directly check for the "first element".
	// Therefore, we check if the "common" field must exist instead.
	if _, ok := parsed["common"]; !ok {
		return "", false
	}

	return raw, true
}
