package HuYa

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/tidwall/gjson"
)

type Link struct {
	rid    string
	uid    string
	uidi   int64
	uuid   string
	res    gjson.Result
	client *http.Client
}

func NewHuyaLink(rid string, proxy *url.URL) (*Link, error) {
	var (
		err error
	)
	hy := new(Link)
	hy.rid = rid
	if proxy != nil {
		hy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, true)}
	} else {
		hy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, true)}
	}
	err = hy.getRoomInfo()
	if err != nil {
		return nil, fmt.Errorf("get room info error: %w", err)
	}
	err = hy.getAnonymousUID()
	if err != nil {
		return nil, fmt.Errorf("get anonymous user id error: %w", err)
	}
	hy.getUUID()
	return hy, nil
}

func (l *Link) GetLink() (*url.URL, error) {
	switch l.res.Get("roomInfo.eLiveStatus").Int() {
	case 2:
		liveInfo, err := l.getLive()
		if err != nil {
			return nil, fmt.Errorf("get live info error: %w", err)
		}
		return url.Parse(liveInfo)
	case 3:
		liveLineURL, err := base64.StdEncoding.DecodeString(l.res.Get("roomProfile.liveLineUrl").String())
		if err != nil {
			return nil, fmt.Errorf("decoding live line url error: %w", err)
		}
		return url.Parse(fmt.Sprintf("https:%s", liveLineURL))
	}
	return nil, errors.New("not streaming now")
}

func (l *Link) getAnonymousUID() (err error) {
	log := global.Log.WithField("function", "app.engine.extractor.HuYa.getAnonymousUID")
	var (
		resp *http.Response
		body []byte
	)
	data := `{
        "appId": 5002,
        "byPass": 3,
        "context": "",
        "version": "2.4",
        "data": {}
    }`
	resp, err = l.client.Post("https://udblgn.huya.com/web/anonymousLogin", "application/json", strings.NewReader(data))
	if err != nil {
		return fmt.Errorf("sending get anonymous uid request error: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing get anonymous uid response body error: %w", err)
	}
	log.WithField("field", "anonymous uid response body").Debug(string(body))
	if !gjson.GetBytes(body, "data.uid").Exists() {
		return errors.New("anonymous user id not found")
	}
	l.uid = gjson.GetBytes(body, "data.uid").String()
	l.uidi = gjson.GetBytes(body, "data.uid").Int()
	return nil
}

func (l *Link) getLive() (string, error) {
	var (
		stream_info []string
	)
	l.res.Get("roomInfo.tLiveInfo.tLiveStreamInfo.vStreamInfo.value").ForEach(func(key, value gjson.Result) bool {
		if value.Get("sFlvUrl").Exists() {
			anticode, err := l.processAntiCode(value.Get("sFlvAntiCode").String(), value.Get("sStreamName").String())
			if err != nil {
				log.Println(fmt.Sprintf("processing anticode error: %s", err.Error()))
				return false
			}
			u, err := url.Parse(fmt.Sprintf("%s/%s.%s?%s",
				value.Get("sFlvUrl").String(),
				value.Get("sStreamName").String(),
				value.Get("sFlvUrlSuffix").String(),
				anticode))
			if err != nil {
				log.Println(err.Error())
				return false
			}
			u.Scheme = "https"
			stream_info = append(stream_info, u.String())
		}
		if value.Get("sHlsUrl").Exists() {
			anticode, err := l.processAntiCode(value.Get("sHlsAntiCode").String(), value.Get("sStreamName").String())
			if err != nil {
				log.Println(fmt.Sprintf("processing anticode error: %s", err.Error()))
				return false
			}
			u, err := url.Parse(fmt.Sprintf("%s/%s.%s?%s",
				value.Get("sHlsUrl").String(),
				value.Get("sStreamName").String(),
				value.Get("sHlsUrlSuffix").String(),
				anticode))
			if err != nil {
				log.Println(err.Error())
				return false
			}
			u.Scheme = "https"
			stream_info = append(stream_info, u.String())
		}
		return true
	})
	if len(stream_info) <= 0 {
		return "", errors.New("no validate link found")
	}
	r := rand.New(rand.NewSource(time.Now().Unix()))

	return stream_info[r.Intn(len(stream_info)-1)], nil
}

func (l *Link) getRoomInfo() (err error) {
	log := global.Log.WithField("function", "app.engine.extractor.HuYa.getRoomInfo")
	var (
		req  *http.Request
		resp *http.Response
		body []byte
	)
	req, err = http.NewRequest("GET", fmt.Sprintf("https://m.huya.com/%s", l.rid), nil)
	if err != nil {
		return fmt.Errorf("making request for get room info error: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", global.DEFAULT_MOBILE_USER_AGENT)
	resp, err = l.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request for get room info error: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	re := regexp.MustCompile(`<script> window.HNF_GLOBAL_INIT = (.*) </script>`)
	result := re.FindStringSubmatch(string(body))
	if len(result) < 2 {
		return errors.New("HNF_GLOBAL_INIT not found")
	}
	log.WithField("field", "room info").Debug(result[1])
	l.res = gjson.Parse(result[1])
	return nil
}

func (l *Link) getUUID() {
	now := time.Now().UnixMilli()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	random := int64(r.Intn(1000-0)+0) | 0
	l.uuid = strconv.FormatInt((now%10000000000*1000+random)%4294967295, 10)
}

func (l *Link) processAntiCode(anticode string, streamname string) (params string, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.HuYa.processAntiCode")
	q, err := url.ParseQuery(anticode)
	if err != nil {
		return "", fmt.Errorf("parsing anticode error: %w", err)
	}
	q.Set("ver", "1")
	q.Set("sv", "2110211124")
	q.Set("seqid", strconv.FormatInt(l.uidi+time.Now().UnixMilli(), 10))
	q.Set("uid", l.uid)
	q.Set("uuid", l.uuid)
	ssb := md5.Sum([]byte(fmt.Sprintf("%s|%s|%s", q.Get("seqid"), q.Get("ctype"), q.Get("t"))))
	ss := hex.EncodeToString(ssb[:])
	fm_orig, err := base64.StdEncoding.DecodeString(q.Get("fm"))
	if err != nil {
		return "", fmt.Errorf("decoding fm error: %w", err)
	}
	fm_orig_str := string(fm_orig)
	log.WithField("field", "decoded fm").Debug(fm_orig_str)
	fm_orig_str = strings.Replace(fm_orig_str, "$0", l.uid, -1)
	fm_orig_str = strings.Replace(fm_orig_str, "$1", streamname, -1)
	fm_orig_str = strings.Replace(fm_orig_str, "$2", ss, -1)
	fm_orig_str = strings.Replace(fm_orig_str, "$3", q.Get("wsTime"), -1)
	wss := md5.Sum([]byte(fm_orig_str))
	q.Set("wsSecret", hex.EncodeToString(wss[:]))
	q.Del("fm")
	if q.Has("txyp") {
		q.Del("txyp")
	}
	return q.Encode(), nil
}
