package BiliBili

import (
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.BiliBili.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("bilibili", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewBiliBiliLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

type Link struct {
	rid    string
	client *http.Client
}

func NewBiliBiliLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.NewBiliBiliLink")
	l := &Link{rid: rid}
	if proxy != nil {
		l.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		l.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	if err := l.resolveRoomID(); err != nil {
		log.WithError(err).Errorln("failed to resolve room ID")
		return nil, err
	}
	log.WithField("rid", l.rid).Infoln("BiliBili extractor created")
	return l, nil
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.Extract")
	if format == "" {
		format = l.DefaultFormat()
	}
	log.WithField("format", format).WithField("rid", l.rid).Debugln("extracting stream")
	u, err := l.GetLink(format)
	if err != nil {
		log.WithError(err).Errorln("failed to get stream link")
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("Referer", "https://live.bilibili.com")
	return &extractor.Result{URL: u.String(), Headers: headers}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"flv", "m3u8"}
}

func (l *Link) DefaultFormat() string {
	return "flv"
}
