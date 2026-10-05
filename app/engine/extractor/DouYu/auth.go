package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

func (l *Link) getDeviceID() (did string, err error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.getDeviceID")
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
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.getEncryptData")
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
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.calculateAuth")
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

// getRateStream calls the getH5PlayV1 API once and, when the request is
// rejected, refreshes the encryption credentials and retries once. The
// enc_data returned by getEncryption embeds an expire_at about 10 minutes
// out, so re-extractions minutes after creation fail with "鉴权失败" unless
// the credentials are renewed.
func (l *Link) getRateStream() (gjson.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.getRateStream")
	data, err := l.doRateStreamRequest()
	if err != nil {
		return gjson.Result{}, err
	}
	if msg := rateStreamError(data); msg != "" {
		log.WithField("field", "rate stream error").Warnf("rate stream api rejected request: %s, refreshing credentials", msg)
		if encErr := l.refreshEncData(); encErr != nil {
			return gjson.Result{}, fmt.Errorf("refresh enc data error after %q: %w", msg, encErr)
		}
		data, err = l.doRateStreamRequest()
		if err != nil {
			return gjson.Result{}, err
		}
	}
	return data, nil
}

// refreshEncData renews the encryption data used to build the auth chain.
func (l *Link) refreshEncData() error {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.refreshEncData")
	if l.encryptDataFn != nil {
		enc, err := l.encryptDataFn()
		if err != nil {
			return err
		}
		l.encData = enc
		return nil
	}
	enc, err := l.getEncryptData()
	if err != nil {
		log.WithError(err).Warnln("failed to refresh encrypt data")
		return err
	}
	l.encData = enc
	log.Infoln("encrypt data refreshed")
	return nil
}

// doRateStreamRequest performs a single getH5PlayV1 POST with a fresh
// timestamp and returns the parsed response.
func (l *Link) doRateStreamRequest() (gjson.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.doRateStreamRequest")
	if l.rateStreamFn != nil {
		return l.rateStreamFn()
	}
	// Refresh the timestamp on every call: the API rejects auth derived from
	// a tt older than its time window ("时间戳错误"), which used to break
	// re-extractions several minutes after the extractor was created.
	l.t10 = strconv.Itoa(int(time.Now().Unix()))
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
