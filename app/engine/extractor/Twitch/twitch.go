package Twitch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
)

const (
	clientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"
	gqlURL   = "https://gql.twitch.tv/gql"
	ua       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

func init() {
	extractor.Register("twitch", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return NewTwitchLink(rid, proxy)
		},
		Mobile:       false,
		InitialError: 500,
	})
}

type Link struct {
	rid    string
	sig    string
	token  string
	client *http.Client
}

func NewTwitchLink(rid string, proxy *url.URL) (*Link, error) {
	tw := &Link{rid: rid}
	if proxy != nil {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false)}
	} else {
		tw.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}
	}
	if err := tw.getSigToken(); err != nil {
		return nil, err
	}
	return tw, nil
}

func (l *Link) Extract(_ string) (*extractor.Result, error) {
	u, err := l.GetLink(l.DefaultFormat())
	if err != nil {
		return nil, err
	}
	return &extractor.Result{URL: u.String()}, nil
}

func (l *Link) SupportedFormats() []string {
	return []string{"m3u8"}
}

func (l *Link) DefaultFormat() string {
	return "m3u8"
}

func (l *Link) GetLink(_ string) (*url.URL, error) {
	params := url.Values{}
	params.Set("allow_source", "true")
	params.Set("allow_audio_only", "true")
	params.Set("allow_spectre", "true")
	params.Set("fast_bread", "true")
	params.Set("p", randP())
	params.Set("play_session_id", randHex(16))
	params.Set("player_backend", "mediaplayer")
	params.Set("playlist_include_framerate", "true")
	params.Set("reassignments_supported", "true")
	params.Set("sig", l.sig)
	params.Set("token", l.token)
	params.Set("cdm", "wv")
	params.Set("player_version", "1.30.0")

	usherURL := fmt.Sprintf("https://usher.ttvnw.net/api/channel/hls/%s.m3u8?%s", l.rid, params.Encode())
	return url.Parse(usherURL)
}

func (l *Link) getSigToken() error {
	payload := map[string]any{
		"operationName": "PlaybackAccessToken_Template",
		"query": `query PlaybackAccessToken_Template($login:String!,$isLive:Boolean!,$vodID:ID!,$isVod:Boolean!,$playerType:String!){streamPlaybackAccessToken(channelName:$login,params:{platform:"web",playerBackend:"mediaplayer",playerType:$playerType})@include(if:$isLive){value signature __typename}videoPlaybackAccessToken(id:$vodID,params:{platform:"web",playerBackend:"mediaplayer",playerType:$playerType})@include(if:$isVod){value signature __typename}}`,
		"variables": map[string]any{
			"isLive":     true,
			"login":      l.rid,
			"isVod":      false,
			"vodID":      "",
			"playerType": "site",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal gql payload: %w", err)
	}

	req, err := http.NewRequest("POST", gqlURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Client-ID", clientID)
	req.Header.Set("Device-ID", randHex(16))
	req.Header.Set("User-Agent", ua)

	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gql request returned status %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			StreamPlaybackAccessToken struct {
				Value     string `json:"value"`
				Signature string `json:"signature"`
			} `json:"streamPlaybackAccessToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode gql response: %w", err)
	}
	if out.Data.StreamPlaybackAccessToken.Signature == "" {
		return fmt.Errorf("empty playback token (channel may be offline or restricted)")
	}
	l.sig = out.Data.StreamPlaybackAccessToken.Signature
	l.token = out.Data.StreamPlaybackAccessToken.Value
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randP() string {
	nBig, _ := rand.Int(rand.Reader, big.NewInt(9_000_000))
	return fmt.Sprintf("%d", nBig.Int64()+1_000_000)
}
