package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	uuidgen "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
)

// GetLink returns a stream URL. The format parameter is accepted for
// interface consistency but DouYu's stream format is determined by the
// server's p2p field; it cannot be selected by the caller.
func (l *Link) GetLink(_ string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.GetLink")
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
