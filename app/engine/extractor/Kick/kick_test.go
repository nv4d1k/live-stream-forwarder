package Kick

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestKick_SupportedFormats(t *testing.T) {
	l := &Link{}
	formats := l.SupportedFormats()
	expected := []string{"m3u8"}
	if len(formats) != len(expected) {
		t.Fatalf("expected %d formats, got %d", len(expected), len(formats))
	}
	for i, f := range formats {
		if f != expected[i] {
			t.Errorf("format[%d]: expected %q, got %q", i, expected[i], f)
		}
	}
}

func TestKick_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "m3u8" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "m3u8")
	}
}

func TestKick_Registry(t *testing.T) {
	entry, ok := extractor.Registry["kick"]
	if !ok {
		t.Fatal("kick not registered in extractor.Registry")
	}
	if entry.Mobile {
		t.Error("Mobile should be false, got true")
	}
	if entry.InitialError != 500 {
		t.Errorf("InitialError = %d, want 500", entry.InitialError)
	}
	if entry.Factory == nil {
		t.Error("Factory should not be nil")
	}
}

// makeJWTWithExp builds a minimal JWT with the given exp claim for testing.
func makeJWTWithExp(exp int64) string {
	header := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(`{"typ":"JWT","alg":"ES384"}`))
	payload := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	signature := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("fakesig"))
	return header + "." + payload + "." + signature
}

func TestKick_Extract_LiveWithPlaybackURL(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	token := makeJWTWithExp(exp)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/channels/testchannel" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q, want %q", got, "application/json")
		}
		if got := r.Header.Get("Referer"); got != "https://kick.com/testchannel" {
			t.Errorf("Referer header = %q, want %q", got, "https://kick.com/testchannel")
		}

		resp := channelResponse{
			ID:          123,
			Slug:        "testchannel",
			PlaybackURL: "https://playback.live-video.net/api/video/v1/test.m3u8?token=" + token,
			Livestream: &struct {
				ID           int64  `json:"id"`
				IsLive       bool   `json:"is_live"`
				SessionTitle string `json:"session_title"`
				ViewerCount  int    `json:"viewer_count"`
			}{
				ID:           456,
				IsLive:       true,
				SessionTitle: "Test Stream",
				ViewerCount:  100,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	l := &Link{
		rid:     "testchannel",
		client:  ts.Client(),
		apiBase: ts.URL,
	}

	result, err := l.Extract("m3u8")
	if err != nil {
		t.Fatalf("Extract() returned error: %v", err)
	}

	expectedURL := "https://playback.live-video.net/api/video/v1/test.m3u8?token=" + token
	if result.URL != expectedURL {
		t.Errorf("URL = %q, want %q", result.URL, expectedURL)
	}

	if got := result.Headers.Get("Referer"); got != "https://kick.com/" {
		t.Errorf("Referer header = %q, want %q", got, "https://kick.com/")
	}
	if got := result.Headers.Get("Origin"); got != "https://kick.com" {
		t.Errorf("Origin header = %q, want %q", got, "https://kick.com")
	}

	// Verify ExpireAt was parsed from JWT.
	if result.ExpireAt == nil {
		t.Fatal("ExpireAt should not be nil")
	}
	expectedExpire := time.Unix(exp, 0)
	if result.ExpireAt.Unix() != expectedExpire.Unix() {
		t.Errorf("ExpireAt = %v, want %v", result.ExpireAt, expectedExpire)
	}
}

func TestKick_Extract_Offline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := channelResponse{
			ID:          123,
			Slug:        "offlinechannel",
			PlaybackURL: "https://playback.live-video.net/api/video/v1/test.m3u8?token=jwt_token",
			Livestream: &struct {
				ID           int64  `json:"id"`
				IsLive       bool   `json:"is_live"`
				SessionTitle string `json:"session_title"`
				ViewerCount  int    `json:"viewer_count"`
			}{
				IsLive: false,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	l := &Link{
		rid:     "offlinechannel",
		client:  ts.Client(),
		apiBase: ts.URL,
	}

	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error for offline channel")
	}
}

func TestKick_Extract_NoLivestream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := channelResponse{
			ID:          123,
			Slug:        "nolivestream",
			PlaybackURL: "https://example.com/test.m3u8",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	l := &Link{
		rid:     "nolivestream",
		client:  ts.Client(),
		apiBase: ts.URL,
	}

	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error when livestream is nil")
	}
}

func TestKick_Extract_NoPlaybackURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := channelResponse{
			ID:          123,
			Slug:        "nourl",
			PlaybackURL: "",
			Livestream: &struct {
				ID           int64  `json:"id"`
				IsLive       bool   `json:"is_live"`
				SessionTitle string `json:"session_title"`
				ViewerCount  int    `json:"viewer_count"`
			}{
				IsLive: true,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	l := &Link{
		rid:     "nourl",
		client:  ts.Client(),
		apiBase: ts.URL,
	}

	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error when playback URL is empty")
	}
}

func TestKick_getChannel_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	l := &Link{
		rid:     "notfound",
		client:  ts.Client(),
		apiBase: ts.URL,
	}

	_, err := l.getChannel()
	if err == nil {
		t.Fatal("getChannel() should return error for 404 response")
	}
}

func TestKick_NewKickLink_WithProxy(t *testing.T) {
	proxyURL, _ := url.Parse("http://proxy:8080")
	l, err := NewKickLink("testchannel", proxyURL)
	if err != nil {
		t.Fatalf("NewKickLink() returned error: %v", err)
	}
	if l.rid != "testchannel" {
		t.Errorf("rid = %q, want %q", l.rid, "testchannel")
	}
	if l.apiBase != defaultAPIBase {
		t.Errorf("apiBase = %q, want %q", l.apiBase, defaultAPIBase)
	}
	if l.client == nil {
		t.Error("client should not be nil")
	}
}

func TestKick_NewKickLink_NoProxy(t *testing.T) {
	l, err := NewKickLink("testchannel", nil)
	if err != nil {
		t.Fatalf("NewKickLink() returned error: %v", err)
	}
	if l.rid != "testchannel" {
		t.Errorf("rid = %q, want %q", l.rid, "testchannel")
	}
	if l.client == nil {
		t.Error("client should not be nil")
	}
}

func TestParseJWTExp_ValidToken(t *testing.T) {
	expectedExp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	token := makeJWTWithExp(expectedExp.Unix())
	playbackURL := "https://playback.live-video.net/api/video/v1/test.m3u8?token=" + token

	exp, err := parseJWTExp(playbackURL)
	if err != nil {
		t.Fatalf("parseJWTExp() returned error: %v", err)
	}
	if exp.Unix() != expectedExp.Unix() {
		t.Errorf("exp = %v, want %v", exp, expectedExp)
	}
}

func TestParseJWTExp_NoToken(t *testing.T) {
	playbackURL := "https://playback.live-video.net/api/video/v1/test.m3u8"
	_, err := parseJWTExp(playbackURL)
	if err == nil {
		t.Fatal("parseJWTExp() should return error when no token parameter")
	}
}

func TestParseJWTExp_InvalidJWT(t *testing.T) {
	playbackURL := "https://playback.live-video.net/api/video/v1/test.m3u8?token=not.a.valid-jwt"
	_, err := parseJWTExp(playbackURL)
	if err == nil {
		t.Fatal("parseJWTExp() should return error for invalid JWT")
	}
}

func TestParseJWTExp_NoExpClaim(t *testing.T) {
	header := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(`{"typ":"JWT","alg":"ES384"}`))
	payload := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(`{"sub":"test"}`))
	signature := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte("fakesig"))
	token := header + "." + payload + "." + signature
	playbackURL := "https://playback.live-video.net/api/video/v1/test.m3u8?token=" + token

	_, err := parseJWTExp(playbackURL)
	if err == nil {
		t.Fatal("parseJWTExp() should return error when JWT has no exp claim")
	}
}

func TestParseJWTExp_ZeroExp(t *testing.T) {
	token := makeJWTWithExp(0)
	playbackURL := "https://playback.live-video.net/api/video/v1/test.m3u8?token=" + token

	_, err := parseJWTExp(playbackURL)
	if err == nil {
		t.Fatal("parseJWTExp() should return error when JWT exp is 0")
	}
}

func TestBase64urlDecode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no padding", "dGVzdA", "test"},
		{"with padding needed", "dGVzdA==", "test"},
		{"already padded", "YQ==", "a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base64urlDecode(tc.input)
			if err != nil {
				t.Fatalf("base64urlDecode() error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("base64urlDecode() = %q, want %q", string(got), tc.want)
			}
		})
	}
}
