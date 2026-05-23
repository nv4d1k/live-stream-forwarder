package HuYa

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"slices"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

func (l *Link) GetLink(format string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.GetLink")
	liveStatus := l.res.Get("roomInfo.eLiveStatus").Int()
	log.WithField("liveStatus", liveStatus).WithField("format", format).Debugln("getting stream link")
	switch liveStatus {
	case 2:
		liveInfo, err := l.getLive(format)
		if err != nil {
			log.WithError(err).Errorln("failed to get live info")
			return nil, fmt.Errorf("get live info error: %w", err)
		}
		return url.Parse(liveInfo)
	case 3:
		liveLineURL, err := base64.StdEncoding.DecodeString(l.res.Get("roomProfile.liveLineUrl").String())
		if err != nil {
			log.WithError(err).Errorln("failed to decode live line url")
			return nil, fmt.Errorf("decoding live line url error: %w", err)
		}
		return url.Parse(fmt.Sprintf("https:%s", liveLineURL))
	}
	log.Warnln("room is not streaming")
	return nil, errors.New("not streaming now")
}

func (l *Link) getLive(format string) (string, error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.getLive")
	var (
		stream_info     []string
		flv_stream_info []string
		hls_stream_info []string
	)
	l.res.Get("roomInfo.tLiveInfo.tLiveStreamInfo.vStreamInfo.value").ForEach(func(key, value gjson.Result) bool {
		/*if value.Get("sHlsUrl").Exists() {
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
			hls_stream_info = append(hls_stream_info, u.String())
		}*/
		if len(stream_info) <= 0 && value.Get("sFlvUrl").Exists() {
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
			flv_stream_info = append(flv_stream_info, u.String())
		}
		return true
	})
	stream_info = slices.Concat(flv_stream_info, hls_stream_info)
	if len(stream_info) <= 0 {
		return "", errors.New("no validate link found")
	}
	r := rand.New(rand.NewSource(time.Now().Unix()))

	switch format {
	case "flv":
		if len(flv_stream_info) <= 0 {
			return "", errors.New("no validate flv link found")
		}
		return flv_stream_info[r.Intn(len(flv_stream_info)-1)], nil
	case "hls":
		if len(hls_stream_info) <= 0 {
			return "", errors.New("no validate hls link found")
		}
		return hls_stream_info[r.Intn(len(hls_stream_info)-1)], nil
	default:
		return stream_info[r.Intn(len(stream_info)-1)], nil
	}
}
