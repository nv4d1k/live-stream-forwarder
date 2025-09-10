package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/dop251/goja"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type Link struct {
	rid        string
	did        string
	t10        string
	t13        string
	apiErrCode int64
	res        string
	proxy      *url.URL

	client *http.Client
}

func NewDouyuLink(rid string, proxy *url.URL) (*Link, error) {
	var (
		err      error
		proxyURL *url.URL
	)
	dy := new(Link)
	dy.t10 = strconv.Itoa(int(time.Now().Unix()))
	dy.t13 = strconv.Itoa(int(time.Now().UnixMilli()))
	if proxy != nil {
		dy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxyURL)}, false)}
		dy.proxy = proxyURL
	} else {
		dy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	dy.rid, err = dy.getRealRoomID(rid)
	if err != nil {
		return nil, fmt.Errorf("get real room id error: %w", err)
	}
	dy.did, err = dy.getDeviceID()
	if err != nil {
		return nil, fmt.Errorf("get device id error: %w", err)
	}
	_, err = dy.getPreData()
	if err != nil {
		return nil, fmt.Errorf("get pre data error: %w", err)
	}
	return dy, nil
}

func (l *Link) GetLink() (*url.URL, error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.GetLink")
	data, err := l.getRateStream()
	log.WithField("field", "rate stream data").Debug(data.Raw)
	if err != nil {
		return nil, fmt.Errorf("get rate stream error: %w", err)
	}
	streamID := strings.Split(filepath.Base(data.Get("data.rtmp_live").String()), ".")[0]
	uuid, _ := uuid.NewUUID()
	s := rand.New(rand.NewSource(time.Now().Unix()))
	playID := l.t13 + "-" + strconv.Itoa(int(math.Floor(s.Float64()*999999998))) + "1"
	switch data.Get("data.p2p").Int() {
	case 0:
		return url.Parse(fmt.Sprintf("%s/%s", data.Get("data.rtmp_url").String(), data.Get("data.rtmp_live").String()))
	case 2:
		txTime := fmt.Sprintf("%x", int64(math.Round((float64(time.Now().UnixMilli())+600000)/1000)))
		streamID := strings.Split(data.Get("data.rtmp_live").String(), ".")[0]
		txSecret := func(txTime, streamID string) string {
			m := md5.Sum([]byte(fmt.Sprintf("5aa5a539c2bea53cd5acc6adf06c8445%s%s", streamID, txTime)))
			return hex.EncodeToString(m[:])
		}(txTime, streamID)
		originURL := fmt.Sprintf("%s/%s&txSecret=%s&txTime=%s&uuid=%s&playid=%s",
			data.Get("data.rtmp_url").String(),
			data.Get("data.rtmp_live").String(),
			txSecret,
			txTime,
			uuid.String(),
			l.t13+"-"+strconv.Itoa(int(math.Floor(s.Float64()*999999998)))+"1",
		)
		u, err := url.Parse(originURL)
		if err != nil {
			return nil, fmt.Errorf("parse origin url error: %w", err)
		}
		u.Host = "hlsh5p2.douyucdn2.cn"
		return url.Parse(strings.ReplaceAll(u.String(), ".flv", ".xs"))
	case 9, 10:
		u := fmt.Sprintf("wss://%s/%s/live/%s&delay=%s&playid=%s&uuid=%s&txSecret=%s&txTime=%s",
			func() string {
				if data.Get("data.p2pMeta.dyxp2p_sug_egde").Exists() {
					return data.Get("data.p2pMeta.dyxp2p_sug_egde").String()
				}
				hostURL := fmt.Sprintf("https://%s/%s.xs?playid=%s&uuid=%s",
					data.Get("data.p2pMeta.xp2p_api_domain").String(),
					streamID,
					playID,
					uuid.String(),
				)
				log.WithField("field", "host url").Debug(hostURL)
				getHostBodyResp, err := l.client.Get(hostURL)
				if err != nil {
					log.WithField("field", "get host body error").Errorln(err.Error())
					return ""
				}
				defer getHostBodyResp.Body.Close()
				hostBody, err := io.ReadAll(getHostBodyResp.Body)
				if err != nil {
					log.WithField("field", "parse host body error").Errorln(err.Error())
					return ""
				}
				log.WithField("field", "host body").Debug(string(hostBody))
				return gjson.GetBytes(hostBody, "sug").Array()[0].String()
			}(),
			func() string {
				if data.Get("data.p2pMeta.dyxp2p_domain").Exists() {
					return data.Get("data.p2pMeta.dyxp2p_domain").String()
				}
				return data.Get("data.p2pMeta.xp2p_domain").String()
			}(),
			data.Get("data.rtmp_live").String(),
			data.Get("data.p2pMeta.xp2p_txDelay").String(),
			playID,
			uuid.String(),
			data.Get("data.p2pMeta.xp2p_txSecret").String(),
			data.Get("data.p2pMeta.xp2p_txTime").String())

		u = strings.ReplaceAll(u, ".flv", ".xs")
		return url.Parse(u)
	}
	return nil, nil
}

func (l *Link) getDeviceID() (did string, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.getDeviceID")
	var (
		req      *http.Request
		resp     *http.Response
		body     []byte
		didRegex = regexp.MustCompile(`axiosJsonpCallback1\((.*)\)`)
	)
	req, err = http.NewRequest("GET", fmt.Sprintf("https://passport.douyu.com/lapi/did/api/get?client_id=25&_=%s&callback=axiosJsonpCallback1", l.t13), nil)
	if err != nil {
		return "", fmt.Errorf("making request error for get device id: %w", err)
	}
	req.Header.Set("Referer", "https://m.douyu.com/")
	resp, err = l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request error for get device id: %w", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("parsing body error for get device id: %w", err)
	}
	log.WithField("field", "device id response body").Debug(string(body))
	didJson := didRegex.FindStringSubmatch(string(body))
	if len(didJson) != 2 {
		return "", errors.New("get device ID error")
	}
	didData := gjson.Parse(didJson[1])
	if didData.Get("error").Int() != 0 {
		return "", errors.New(didData.Get("msg").String())
	}
	return didData.Get("data.did").String(), nil
}

func (l *Link) getRateStream() (gjson.Result, error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.getRateStream")
	z := html.NewTokenizer(strings.NewReader(l.res))
	var scripts []string
	var stag bool
FIND:
	for {
		t := z.Next()
		switch t {
		case html.ErrorToken:
			break FIND
		case html.StartTagToken:
			x := z.Token()
			stag = x.Data == "script"
		case html.TextToken:
			x := z.Token()
			if stag {
				scripts = append(scripts, x.Data)
			}
			stag = false
		}
	}
	var jj string
	for _, c := range scripts {
		if strings.Contains(c, "ub98484234") {
			jj = c
		}
	}
	if jj == "" {
		return gjson.Result{}, fmt.Errorf("could not find ub98484234 function")
	}

	// Step 2: Modify the JavaScript function to remove eval statement
	replaceRe := regexp.MustCompile(`return\s*eval\(strc\)\([0-9a-z]+,[0-9a-z]+,[0-9a-z]+\);}`)
	jsFunc := replaceRe.ReplaceAllString(jj, "return strc;}")

	// Step 3: Compile and execute the modified JavaScript function
	js := fmt.Sprintf("%s\nub98484234()", jsFunc)

	vm := goja.New()
	vmResult, err := vm.RunString(js)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("running ub98484234 error: %w", err)
	}
	result := vmResult.String()
	log.WithField("field", "result of running ub98484234").Debug(result)
	re := regexp.MustCompile(`v=(\d+)`)
	match := re.FindStringSubmatch(result)
	if len(match) < 2 {
		return gjson.Result{}, fmt.Errorf("could not find v parameter")
	}
	v := match[1]
	// Step 5: Generate rb parameter using md5 function
	rbByte := md5.Sum([]byte(fmt.Sprintf("%s%s%s%s", l.rid, l.did, l.t10, v)))
	rb := hex.EncodeToString(rbByte[:])

	// Step 6: Modify JavaScript function to replace return statement with rb parameter
	jsFunc = strings.Replace(result, "return rt;})", "return rt;}", -1)
	jsFunc = strings.Replace(jsFunc, "(function (", "function sign(", -1)
	jsFunc = strings.Replace(jsFunc, "CryptoJS.MD5(cb).toString()", fmt.Sprintf("\"%s\"", rb), -1)
	jsFunc = fmt.Sprintf("%s sign(%s, \"%s\", %s);", jsFunc, l.rid, l.did, l.t10)

	vmSignResult, err := vm.RunString(jsFunc)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("running signature function error: %w", err)
	}
	result = vmSignResult.String()
	params := fmt.Sprintf("%s&ver=Douyu_223092005&rate=0&cdn=&iar=0&ive=0&hevc=0&fa=0", result)
	url := fmt.Sprintf("https://playweb.douyu.com/lapi/live/getH5Play/%s", l.rid)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(params))
	if err != nil {
		return gjson.Result{}, fmt.Errorf("making RateStream POST request error: %w", err)
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("sending RateStream POST request error: %w", err)
	}
	defer resp.Body.Close()

	// Step 8: Parse the JSON response and return it
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("getting RateStream response error: %w", err)
	}

	log.WithField("field", "rate stream").Debug(string(body))
	return gjson.ParseBytes(body), nil
}

func (l *Link) getRealRoomID(rfid string) (rid string, err error) {
	ridRegex := regexp.MustCompile(`ROOM\.room_id\s?=\s?(\d{1,8});`)
	resp, err := l.client.Get(fmt.Sprintf("https://www.douyu.com/%s", rfid))
	if err != nil {
		return "", fmt.Errorf("send request error when getting real room id: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("parsing response body error when getting real room id: %w", err)
	}
	l.res = string(body)
	rids := ridRegex.FindStringSubmatch(l.res)
	if len(rids) != 2 {
		return "", errors.New("invalidate room id")
	}
	return rids[1], nil
}

func (l *Link) getPreData() (errorCode int64, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.getPreData")
	var (
		req  *http.Request
		resp *http.Response
		body []byte
	)
	data := url.Values{}
	data.Add("rid", l.rid)
	data.Add("did", l.did)
	hash := md5.Sum([]byte(fmt.Sprintf("%s%s", l.rid, l.t13)))
	auth := hex.EncodeToString(hash[:])
	req, err = http.NewRequest("POST", fmt.Sprintf("https://playweb.douyucdn.cn/lapi/live/hlsH5Preview/%s", l.rid), strings.NewReader(data.Encode()))
	if err != nil {
		return 0, fmt.Errorf("making request error: %w", err)
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("rid", l.rid)
	req.Header.Add("time", l.t13)
	req.Header.Add("auth", auth)
	resp, err = l.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post for predata error: %w", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("parse body error for get predata: %w", err)
	}
	log.WithField("field", "predata body").Debug(string(body))
	l.apiErrCode = gjson.GetBytes(body, "error").Int()
	switch gjson.GetBytes(body, "error").Int() {
	case 0, 742017:
		return gjson.GetBytes(body, "error").Int(), nil
	default:
		return gjson.GetBytes(body, "error").Int(), fmt.Errorf("%s(%d)", gjson.GetBytes(body, "msg").String(), gjson.GetBytes(body, "error").Int())
	}
}
