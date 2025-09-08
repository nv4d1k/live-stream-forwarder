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
	//reTtwid := regexp.MustCompile(`(?i)ttwid=(\S*);`)

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
		/*
				return l.getCookies()
			case reTtwid.MatchString(resp.Header.Get("Set-Cookie")):
				ttwid := reTtwid.FindStringSubmatch(resp.Header.Get("Set-Cookie"))[1]
				l.cookies = &http.Cookie{Name: "ttwid", Value: ttwid}
		*/
		return nil
	default:
		return errors.New("both __ac_nonce and ttwid are not found")
	}
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
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request for get link error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	doc, err := htmlquery.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parsing response body error: %w", err)
	}
	var liveData string
	nodes := htmlquery.Find(doc, "//script")
	for _, node := range nodes {
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
	log.WithField("field", "stream data").Debug(stream.Raw)
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
	re := regexp.MustCompile(`self\.__pace_f\.push\(\[1,"{(.*)}"\]\)`)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 2 {
		return "", false
	}

	// matches[1] 是带转义的 JSON，比如 {\"key\":\"val\"}
	escaped := matches[1]

	// 用 json.Unmarshal 去掉转义
	var result string
	if err := json.Unmarshal([]byte(`"`+escaped+`"`), &result); err != nil {
		return "", false
	}

	return fmt.Sprintf("{%s}", result), true
}
