package HuYa

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

func init() {
	if global.Log != nil {
		log := global.Log.WithField("func", "app.engine.extractor.HuYa.init")
		log.Infoln("registering extractor")
	}
	extractor.Register("huya", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewHuyaLink(rid, proxy)
		},
		Mobile:       true,
		InitialError: 500,
	})
}

type Link struct {
	rid    string
	uid    string
	uidi   int64
	uuid   string
	res    gjson.Result
	client *http.Client
}

func NewHuyaLink(rid string, proxy *url.URL) (*Link, error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.NewHuyaLink")
	var (
		err error
	)
	hy := new(Link)
	hy.rid = rid
	log.WithField("rid", rid).Infoln("creating HuYa extractor")
	if proxy != nil {
		hy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, true)}
	} else {
		hy.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, true)}
	}
	err = hy.getRoomInfo()
	if err != nil {
		log.WithError(err).Errorln("failed to get room info")
		return nil, fmt.Errorf("get room info error: %w", err)
	}
	err = hy.getAnonymousUID()
	if err != nil {
		log.WithError(err).Errorln("failed to get anonymous user id")
		return nil, fmt.Errorf("get anonymous user id error: %w", err)
	}
	hy.getUUID()
	log.Infoln("HuYa extractor created successfully")
	return hy, nil
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.Extract")
	if format == "" {
		format = l.DefaultFormat()
	}
	// Normalize "m3u8" to "hls" for HuYa's internal API.
	if format == "m3u8" {
		log.Debugln("normalizing format from m3u8 to hls")
		format = "hls"
	}
	u, err := l.GetLink(format)
	if err != nil {
		log.WithError(err).Errorln("failed to get stream link")
		return nil, err
	}
	log.WithField("url", u.String()).Infoln("extracted stream URL")
	return &extractor.Result{URL: u.String()}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"flv", "hls"}
}

func (l *Link) DefaultFormat() string {
	return "flv"
}
