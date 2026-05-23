package Twitch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

const (
	clientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"
	gqlURL   = "https://gql.twitch.tv/gql"
	ua       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

func (l *Link) getSigToken() error {
	log := global.Log.WithField("func", "app.engine.extractor.Twitch.getSigToken")
	log.WithField("rid", l.rid).Debugln("requesting playback access token")
	payload := map[string]any{
		"operationName": "PlaybackAccessToken_Template",
		"query":         `query PlaybackAccessToken_Template($login:String!,$isLive:Boolean!,$vodID:ID!,$isVod:Boolean!,$playerType:String!){streamPlaybackAccessToken(channelName:$login,params:{platform:"web",playerBackend:"mediaplayer",playerType:$playerType})@include(if:$isLive){value signature __typename}videoPlaybackAccessToken(id:$vodID,params:{platform:"web",playerBackend:"mediaplayer",playerType:$playerType})@include(if:$isVod){value signature __typename}}`,
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
		log.Errorf("gql request failed for room %s: %v", l.rid, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warnf("gql request returned status %d for room %s", resp.StatusCode, l.rid)
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
		log.Errorf("failed to decode gql response for room %s: %v", l.rid, err)
		return fmt.Errorf("decode gql response: %w", err)
	}
	if out.Data.StreamPlaybackAccessToken.Signature == "" {
		log.Warnf("empty playback token for room %s (channel may be offline or restricted)", l.rid)
		return fmt.Errorf("empty playback token (channel may be offline or restricted)")
	}
	l.sig = out.Data.StreamPlaybackAccessToken.Signature
	l.token = out.Data.StreamPlaybackAccessToken.Value
	log.Debugf("obtained playback access token for room %s", l.rid)
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
