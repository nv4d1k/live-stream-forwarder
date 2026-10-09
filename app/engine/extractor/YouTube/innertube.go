package YouTube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// Innertube client constants. YouTube gates streams per client; the ANDROID
// client is the one verified to return streamingData.hlsManifestUrl for live
// streams without cookies or PO tokens. Keep these in one place so a future
// policy change only requires updating this block.
const (
	defaultAPIBase = "https://www.youtube.com"

	innertubeAPIKey     = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"
	innertubePlayerPath = "/youtubei/v1/player"

	androidClientName    = "ANDROID"
	androidClientVersion = "20.10.38"
	androidSDKVersion    = 35
	androidOSName        = "Android"
	androidOSVersion     = "15"
	androidUserAgent     = "com.google.android.youtube/20.10.38 (Linux; U; Android 15) gzip"
)

// innertubeClient is the client descriptor sent in the player request body.
type innertubeClient struct {
	ClientName        string `json:"clientName"`
	ClientVersion     string `json:"clientVersion"`
	AndroidSDKVersion int    `json:"androidSdkVersion,omitempty"`
	OSName            string `json:"osName,omitempty"`
	OSVersion         string `json:"osVersion,omitempty"`
	HL                string `json:"hl,omitempty"`
	GL                string `json:"gl,omitempty"`
	UserAgent         string `json:"userAgent,omitempty"`
}

// playerRequest is the innertube /player request payload.
type playerRequest struct {
	Context struct {
		Client innertubeClient `json:"client"`
	} `json:"context"`
	VideoID        string `json:"videoId"`
	ContentCheckOK bool   `json:"contentCheckOk"`
	RacyCheckOK    bool   `json:"racyCheckOk"`
}

// playerResponse carries only the fields the extractor consumes.
type playerResponse struct {
	VideoDetails struct {
		VideoID string `json:"videoId"`
		Title   string `json:"title"`
		IsLive  bool   `json:"isLive"`
	} `json:"videoDetails"`
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	StreamingData struct {
		HLSManifestURL  string `json:"hlsManifestUrl"`
		DASHManifestURL string `json:"dashManifestUrl"`
	} `json:"streamingData"`
}

// getPlayer calls the innertube player API with the ANDROID client and
// returns the parsed response. The request carries no cookies.
func (l *Link) getPlayer() (*playerResponse, error) {
	log := global.Log.WithField("func", "app.engine.extractor.YouTube.getPlayer")

	var reqBody playerRequest
	reqBody.Context.Client = innertubeClient{
		ClientName:        androidClientName,
		ClientVersion:     androidClientVersion,
		AndroidSDKVersion: androidSDKVersion,
		OSName:            androidOSName,
		OSVersion:         androidOSVersion,
		HL:                "en",
		GL:                "US",
		UserAgent:         androidUserAgent,
	}
	reqBody.VideoID = l.rid
	reqBody.ContentCheckOK = true
	reqBody.RacyCheckOK = true

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal player request: %w", err)
	}

	apiURL := l.apiBase + innertubePlayerPath + "?key=" + innertubeAPIKey + "&prettyPrint=false"
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create player request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", androidUserAgent)
	req.Header.Set("X-YouTube-Client-Name", "3")
	req.Header.Set("X-YouTube-Client-Version", androidClientVersion)
	log.Debugf("requesting player API for room %s", l.rid)

	resp, err := l.client.Do(req)
	if err != nil {
		log.Errorf("player API request failed for room %s: %v", l.rid, err)
		return nil, fmt.Errorf("request player API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warnf("player API returned status %d for room %s", resp.StatusCode, l.rid)
		return nil, fmt.Errorf("player API returned status %d", resp.StatusCode)
	}

	var pr playerResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		log.Errorf("failed to decode player response for room %s: %v", l.rid, err)
		return nil, fmt.Errorf("decode player response: %w", err)
	}
	log.Debugf("got player response for room %s (playability=%s isLive=%v hls=%v)",
		l.rid, pr.PlayabilityStatus.Status, pr.VideoDetails.IsLive, pr.StreamingData.HLSManifestURL != "")
	return &pr, nil
}
