package HuYa

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

func (l *Link) processAntiCode(anticode string, streamname string) (params string, err error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.processAntiCode")
	q, err := url.ParseQuery(anticode)
	if err != nil {
		return "", fmt.Errorf("parsing anticode error: %w", err)
	}
	q.Set("ver", "1")
	q.Set("sv", "202412311405")
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
