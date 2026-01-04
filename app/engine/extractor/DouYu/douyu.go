package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"

	"github.com/iceking2nd/go-toolkits/converter"
	uuidgen "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
)

type Link struct {
	rid          string
	did          string
	encData      string
	t10          string
	t13          string
	apiErrCode   int64
	res          string
	streamParams streamParameters
	proxy        *url.URL

	client *http.Client
}

type streamParameters struct {
	RoomID    uint64 `json:"roomID"`
	Nonce     string `json:"nonce"`
	OwnerUID  uint64 `json:"owner_uid"`
	CookiePre string `json:"cookiePre"`
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
	dy.streamParams, err = dy.getLegacyFirstStreamParameters(rid)
	if err != nil {
		return nil, fmt.Errorf("get real room id error: %w", err)
	}
	dy.rid = fmt.Sprintf("%d", dy.streamParams.RoomID)
	dy.did, err = dy.getDeviceID()
	if err != nil {
		return nil, fmt.Errorf("get device id error: %w", err)
	}
	dy.encData, err = dy.getEncryptData()
	if err != nil {
		return nil, fmt.Errorf("get encrypt data error: %w", err)
	}
	return dy, nil
}

func (l *Link) GetLink() (*url.URL, error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.GetLink")
	data, err := l.getRateStream()
	log.WithField("data", data.Raw).Debugln("rate stream data")
	if err != nil {
		return nil, fmt.Errorf("get rate stream error: %w", err)
	}
	streamID := strings.Split(filepath.Base(data.Get("data.rtmp_live").String()), ".")[0]
	uuid := uuidgen.NewV4()
	s := rand.New(rand.NewSource(time.Now().Unix()))
	playID := l.t13 + "-" + strconv.Itoa(int(math.Floor(s.Float64()*999999998))) + "1"
	switch data.Get("data.p2p").Int() {
	case 0, 9:
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
	case 10:
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

func (l *Link) getEncryptData() (encData string, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.getEncryptData")
	if len(l.did) <= 0 {
		return "", errors.New("did is empty")
	}
	resp, err := l.client.Get(fmt.Sprintf("https://www.douyu.com/wgapi/livenc/liveweb/websec/getEncryption?did=%s", l.did))
	if err != nil {
		return "", fmt.Errorf("sending request error for get encrypt data: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("parsing body error for get encrypt data: %w", err)
	}
	encData = gjson.GetBytes(body, "data").String()
	log.WithField("encrypt data", encData).Debugln("get encrypt data")
	return encData, nil
}

func (l *Link) calculateAuth() (auth string, err error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.calculateAuth")
	encData := gjson.Parse(l.encData)
	key := encData.Get("key").String()
	randStr := encData.Get("rand_str").String()
	encTime := encData.Get("enc_time").Int()
	authString := randStr
	for i := int64(0); i < encTime; i++ {
		log.WithField("encrypt_time", i).WithField("auth_string", authString).WithField("key", key).Debugln("calculate auth in loop")
		hash := md5.Sum([]byte(fmt.Sprintf("%s%s", authString, key)))
		authString = hex.EncodeToString(hash[:])
	}
	log.WithField("authString", authString).WithField("key", key).WithField("room_id", l.rid).WithField("timestamp", l.t10).Debugln("calculate auth after loop")
	hash := md5.Sum([]byte(fmt.Sprintf("%s%s%s", authString, key, fmt.Sprintf("%s%s", l.rid, l.t10))))
	auth = hex.EncodeToString(hash[:])
	log.WithField("auth", auth).Debugln("calculate authString")
	return auth, nil
}

func (l *Link) getRateStream() (gjson.Result, error) {
	log := global.Log.WithField("function", "app.engine.extractor.DouYu.getRateStream")
	auth, err := l.calculateAuth()
	if err != nil {
		return gjson.Result{}, fmt.Errorf("calculate auth error for get rate stream: %w", err)
	}
	params := url.Values{}
	params.Set("auth", auth)
	params.Set("enc_data", gjson.Get(l.encData, "enc_data").String())
	params.Set("tt", l.t10)
	params.Set("did", l.did)
	params.Set("rate", "0")
	params.Set("cdn", "")
	params.Set("ive", "0")
	params.Set("hevc", "1")
	params.Set("fa", "0")
	log.WithField("params", params.Encode()).Debugln("get rate stream")
	rateStreamUrl := fmt.Sprintf("https://www.douyu.com/lapi/live/getH5PlayV1/%s", l.rid)
	req, err := http.NewRequest(http.MethodPost, rateStreamUrl, strings.NewReader(params.Encode()))
	if err != nil {
		return gjson.Result{}, fmt.Errorf("making RateStream POST request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", fmt.Sprintf("https://www.douyu.com/%s", l.rid))
	req.Header.Set("Origin", "https://www.douyu.com")
	log.WithField("headers", req.Header).Debugln("get rate stream")

	resp, err := l.client.Do(req)
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

func (l *Link) getLegacyFirstStreamParameters(rfid string) (sp streamParameters, err error) {
	streamParamRegex := regexp.MustCompile(`window.preloadStreamUrlPromise = getLegacyFirstStream\((.*)\)\s?;`)
	resp, err := l.client.Get(fmt.Sprintf("https://www.douyu.com/%s", rfid))
	if err != nil {
		return sp, fmt.Errorf("send request error when getting real room id: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sp, fmt.Errorf("parsing response body error when getting real room id: %w", err)
	}
	l.res = string(body)
	streamParam := streamParamRegex.FindStringSubmatch(l.res)
	if len(streamParam) != 2 {
		return sp, errors.New("invalidate stream parameters")
	}
	spjson, err := converter.JSObj2JSON(streamParam[1])
	if err != nil {
		return sp, fmt.Errorf("parsing stream parameters error: %w", err)
	}
	err = json.Unmarshal(spjson, &sp)
	return sp, nil
}
