package Twitch

import (
	"errors"
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

type Link struct {
	rid    string
	cid    string
	sig    string
	token  string
	client *http.Client
}

func NewTwitchLink(rid string, proxy *url.URL) (*Link, error) {
	var (
		err error
	)
	tw := new(Link)
	tw.rid = rid
	if proxy != nil {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	err = tw.getClientID()
	if err != nil {
		return nil, err
	}
	err = tw.getSigToken()
	if err != nil {
		return nil, err
	}
	return tw, nil
}

func (l *Link) GetLink() (*url.URL, error) {
	params := url.Values{}
	params.Add("allow_source", "true")
	params.Add("fast_bread", "true")
	params.Add("player_backend", "mediaplayer")
	params.Add("playlist_include_framerate", "true")
	params.Add("reassignments_supported", "true")
	params.Add("sig", l.sig)
	params.Add("supported_codecs", "vp09,avc1")
	params.Add("token", l.token)
	params.Add("cdm", "wv")
	params.Add("player_version", "1.18.0")
	stream_info := map[string]string{}
	stream_info["m3u8"] = fmt.Sprintf("https://usher.ttvnw.net/api/channel/hls/%s.m3u8?%s", l.rid, params.Encode())
	return url.Parse(stream_info["m3u8"])
}

func (l *Link) getClientID() error {
	resp, err := l.client.Get(fmt.Sprintf("https://www.twitch.tv/%s", l.rid))
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`clientId="(.*?)"`)
	cid := re.FindStringSubmatch(string(body))
	if len(cid) < 2 {
		return errors.New("client id not found")
	}
	l.cid = cid[1]
	return nil
}

func (l *Link) getSigToken() error {
	data := fmt.Sprintf(`{
  "operationName": "PlaybackAccessToken_Template",
  "query": "query PlaybackAccessToken_Template($login: String!, $isLive: Boolean!, $vodID: ID!, $isVod: Boolean!, $playerType: String!) {  streamPlaybackAccessToken(channelName: $login, params: {platform: \"web\", playerBackend: \"mediaplayer\", playerType: $playerType}) @include(if: $isLive) {    value    signature    __typename  }  videoPlaybackAccessToken(id: $vodID, params: {platform: \"web\", playerBackend: \"mediaplayer\", playerType: $playerType}) @include(if: $isVod) {    value    signature    __typename  }}",
  "variables": {
    "isLive": true,
    "login": "%s",
    "isVod": false,
    "vodID": "",
    "playerType": "site"
  }
}`, l.rid)
	req, err := http.NewRequest("POST", "https://gql.twitch.tv/gql", strings.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", fmt.Sprintf("https://www.twitch.tv/%s", l.rid))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.93 Safari/537.36")
	req.Header.Add("Client-ID", l.cid)
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	result := gjson.ParseBytes(body)
	l.sig = result.Get("data.streamPlaybackAccessToken.signature").String()
	l.token = result.Get("data.streamPlaybackAccessToken.value").String()
	return nil
}
