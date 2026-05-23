package DouYin

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.DouYin.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("douyin", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewDouYinLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

type Link struct {
	rid string

	cookies *http.Cookie
	client  *http.Client
}

func NewDouYinLink(rid string, proxy *url.URL) (douyin *Link, err error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.NewDouYinLink")
	douyin = new(Link)
	douyin.rid = rid
	log.WithField("rid", rid).Infoln("creating DouYin extractor")
	if proxy != nil {
		douyin.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		douyin.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	douyin.cookies = &http.Cookie{}
	err = douyin.getCookies()
	if err != nil {
		log.WithError(err).Errorln("failed to get cookies")
		return nil, fmt.Errorf("get cookies error: %w", err)
	}
	return douyin, nil
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYin.Extract")
	if format == "" {
		format = l.DefaultFormat()
	}
	log.WithField("format", format).Infoln("extracting stream URL")
	u, err := l.GetLink(format)
	if err != nil {
		log.WithError(err).Errorln("failed to get stream URL")
		return nil, err
	}
	log.WithField("url", u.String()).Debugln("extracted stream URL")
	return &extractor.Result{URL: u.String()}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"flv", "m3u8"}
}

func (l *Link) DefaultFormat() string {
	return "flv"
}
