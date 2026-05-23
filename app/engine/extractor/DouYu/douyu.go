package DouYu

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.DouYu.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("douyu", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewDouyuLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 400,
	})
}

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

func NewDouyuLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.NewDouyuLink")
	log.WithField("rid", rid).Infoln("creating DouYu extractor")
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
		log.WithError(err).Errorln("failed to get stream parameters")
		return nil, fmt.Errorf("get real room id error: %w", err)
	}
	dy.rid = fmt.Sprintf("%d", dy.streamParams.RoomID)
	log.WithField("rid", dy.rid).Debugln("resolved real room id")
	dy.did, err = dy.getDeviceID()
	if err != nil {
		log.WithError(err).Errorln("failed to get device id")
		return nil, fmt.Errorf("get device id error: %w", err)
	}
	dy.encData, err = dy.getEncryptData()
	if err != nil {
		log.WithError(err).Errorln("failed to get encrypt data")
		return nil, fmt.Errorf("get encrypt data error: %w", err)
	}
	log.WithField("rid", dy.rid).Infoln("DouYu extractor created successfully")
	return dy, nil
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.Extract")
	log.WithField("format", format).Debugln("extracting stream URL")
	u, err := l.GetLink(format)
	if err != nil {
		log.WithError(err).Errorln("failed to get stream link")
		return nil, err
	}
	log.WithField("url", u.String()).Infoln("stream URL extracted")
	return &extractor.Result{URL: u.String()}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"flv", "m3u8", "ws"}
}

func (l *Link) DefaultFormat() string {
	return "flv"
}
