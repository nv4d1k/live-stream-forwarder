package BiliBili

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

func (l *Link) resolveRoomID() error {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.resolveRoomID")
	resp, err := l.client.Get(fmt.Sprintf("https://api.live.bilibili.com/room/v1/Room/room_init?id=%s", l.rid))
	if err != nil {
		return fmt.Errorf("get room init info error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read room init info error: %w", err)
	}
	log.WithField("field", "room init data").Debug(string(body))
	var result roomInitResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse room init info error: %w", err)
	}
	if result.Code != 0 {
		return errors.New("live room does not exist")
	}
	l.rid = fmt.Sprintf("%d", result.Data.RoomID)
	if result.Data.LiveStatus != 1 {
		return errors.New("live room is offline")
	}
	if result.Data.IsLocked {
		return errors.New("live room is locked")
	}
	if result.Data.Encrypted {
		return errors.New("live room is encrypted (password required)")
	}
	return nil
}

func (l *Link) GetLink(format string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.GetLink")

	// v1 API: returns direct stream URLs, often with higher quality for
	// unauthenticated users than v2.
	if u, err := l.getLinkByAPIv1(format); err == nil {
		log.WithField("field", "stream url (v1)").Debug(u.String())
		return u, nil
	}

	// Fallback to v2 API.
	playInfo, err := l.getPlayInfo()
	if err != nil {
		return nil, err
	}
	streamURL, err := selectStreamURL(playInfo, format)
	if err != nil {
		return nil, err
	}
	log.WithField("field", "stream url (v2)").Debug(streamURL)
	return url.Parse(streamURL)
}

func (l *Link) getLinkByAPIv1(format string) (*url.URL, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.getLinkByAPIv1")
	platform := "web"
	if format == "m3u8" || format == "hls" {
		platform = "h5"
	}
	resp, err := l.client.Get(fmt.Sprintf(
		"https://api.live.bilibili.com/room/v1/Room/playUrl?cid=%s&platform=%s&qn=10000",
		l.rid, platform,
	))
	if err != nil {
		return nil, fmt.Errorf("v1 api request error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("v1 api read body error: %w", err)
	}
	log.WithField("field", "v1 response").Debug(string(body))
	var result playURLV1Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("v1 api parse error: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("v1 api error code %d", result.Code)
	}
	if len(result.Data.Durl) == 0 {
		return nil, errors.New("v1 api returned no streams")
	}
	// Pick a random CDN node.
	du := result.Data.Durl[rand.Intn(len(result.Data.Durl))]
	if du.URL == "" {
		return nil, errors.New("v1 api returned empty url")
	}
	return url.Parse(du.URL)
}

func (l *Link) getPlayInfo() (*playInfoResponse, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.getPlayInfo")
	u, _ := url.Parse("https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo")
	q := u.Query()
	q.Set("room_id", l.rid)
	q.Set("protocol", "0,1")
	q.Set("format", "0,1,2")
	q.Set("codec", "0,1")
	q.Set("qn", "10000")
	q.Set("platform", "web")
	q.Set("ptype", "8")
	q.Set("dolby", "5")
	q.Set("panorama", "1")
	u.RawQuery = q.Encode()
	log.WithField("field", "play info url").Debug(u.String())
	resp, err := l.client.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("get play info error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read play info error: %w", err)
	}
	log.WithField("field", "play info").Debug(string(body))
	var result playInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse play info error: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("play info api error code %d: %s", result.Code, result.Message)
	}
	if len(result.Data.PlayURLInfo.PlayURL.Streams) == 0 {
		return nil, errors.New("no streams available")
	}
	return &result, nil
}

// selectStreamURL picks the best stream URL based on the requested format.
// For "flv": selects http_stream/flv protocol.
// For "m3u8": selects http_hls protocol, preferring fmp4 over ts.
// Codec preference: avc (most compatible) > hevc.
// CDN node: randomly selected from url_info for load balancing.
func selectStreamURL(info *playInfoResponse, format string) (string, error) {
	log := global.Log.WithField("func", "app.engine.extractor.BiliBili.selectStreamURL")
	streams := info.Data.PlayURLInfo.PlayURL.Streams

	log.WithField("format", format).Debugln("selecting stream URL")

	var targetProtocol, targetFormat, fallbackFormat string
	switch format {
	case "m3u8", "hls":
		targetProtocol = "http_hls"
		targetFormat = "fmp4"
		fallbackFormat = "ts"
	default:
		targetProtocol = "http_stream"
		targetFormat = "flv"
	}

	// Find the matching protocol.
	var targetStream *streamItem
	for i := range streams {
		if streams[i].ProtocolName == targetProtocol {
			targetStream = &streams[i]
			break
		}
	}
	if targetStream == nil {
		log.WithField("protocol", targetProtocol).Warnln("no matching protocol stream found")
		return "", fmt.Errorf("no %s stream available", targetProtocol)
	}

	// Find the matching format, with fallback for HLS.
	var targetFmt *formatItem
	for i := range targetStream.Formats {
		if targetStream.Formats[i].FormatName == targetFormat {
			targetFmt = &targetStream.Formats[i]
			break
		}
	}
	if targetFmt == nil && fallbackFormat != "" {
		for i := range targetStream.Formats {
			if targetStream.Formats[i].FormatName == fallbackFormat {
				targetFmt = &targetStream.Formats[i]
				break
			}
		}
		if targetFmt != nil {
			log.WithField("fallbackFormat", fallbackFormat).Debugln("using fallback format")
		}
	}
	if targetFmt == nil {
		return "", fmt.Errorf("no %s/%s stream available", targetProtocol, targetFormat)
	}

	// Pick the best codec: prefer avc for compatibility, then highest current_qn.
	var bestCodec *codecItem
	for i := range targetFmt.Codecs {
		c := &targetFmt.Codecs[i]
		if bestCodec == nil {
			bestCodec = c
			continue
		}
		if c.CodecName == "avc" && bestCodec.CodecName != "avc" {
			bestCodec = c
		} else if c.CodecName == bestCodec.CodecName && c.CurrentQn > bestCodec.CurrentQn {
			bestCodec = c
		}
	}
	if bestCodec == nil || len(bestCodec.URLInfo) == 0 {
		return "", errors.New("no codec/URL available for selected stream")
	}

	// Build the full URL from a random CDN node.
	ui := bestCodec.URLInfo[rand.Intn(len(bestCodec.URLInfo))]
	result := ui.Host + bestCodec.BaseURL + ui.Extra
	log.WithField("codec", bestCodec.CodecName).WithField("protocol", targetProtocol).WithField("format", targetFormat).Debugln("stream URL selected")
	return result, nil
}
