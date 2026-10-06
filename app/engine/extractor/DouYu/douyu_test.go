package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestDouYu_SupportedFormats(t *testing.T) {
	l := &Link{}
	formats := l.SupportedFormats()
	expected := []string{"flv", "m3u8", "ws"}
	if len(formats) != len(expected) {
		t.Fatalf("expected %d formats, got %d", len(expected), len(formats))
	}
	for i, f := range formats {
		if f != expected[i] {
			t.Errorf("format[%d]: expected %q, got %q", i, expected[i], f)
		}
	}
}

func TestDouYu_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "flv" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "flv")
	}
}

func TestDouYu_Registry(t *testing.T) {
	entry, ok := extractor.Registry["douyu"]
	if !ok {
		t.Fatal("douyu not registered in extractor.Registry")
	}
	if entry.Mobile {
		t.Error("Mobile should be false, got true")
	}
	if entry.InitialError != 400 {
		t.Errorf("InitialError = %d, want 400", entry.InitialError)
	}
	if entry.Factory == nil {
		t.Error("Factory should not be nil")
	}
}

func TestDouYu_CalcAuth(t *testing.T) {
	// Test the auth calculation chain with known values to verify
	// the MD5 loop and final hash are computed correctly.
	key := "testkey123"
	randStr := "randomstr456"
	encTime := int64(2)
	rid := "12345"
	t10 := "1000000000"

	l := &Link{
		rid: rid,
		t10: t10,
		did: "testdid",
		encData: fmt.Sprintf(`{
			"key": "%s",
			"rand_str": "%s",
			"enc_time": %d,
			"expire_at": 1767550175,
			"is_special": 0,
			"enc_data": "dummy"
		}`, key, randStr, encTime),
	}

	auth, err := l.calculateAuth()
	if err != nil {
		t.Fatalf("calculateAuth() returned error: %v", err)
	}

	// Manually compute the expected auth value
	authString := randStr
	for i := int64(0); i < encTime; i++ {
		hash := md5.Sum([]byte(fmt.Sprintf("%s%s", authString, key)))
		authString = hex.EncodeToString(hash[:])
	}
	finalHash := md5.Sum([]byte(fmt.Sprintf("%s%s%s", authString, key, fmt.Sprintf("%s%s", rid, t10))))
	expected := hex.EncodeToString(finalHash[:])

	if auth != expected {
		t.Errorf("calculateAuth() = %q, want %q", auth, expected)
	}

	// Verify auth is a 32-character lowercase hex string
	if len(auth) != 32 {
		t.Errorf("auth length = %d, want 32", len(auth))
	}
	for _, c := range auth {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("auth contains non-hex character: %c", c)
			break
		}
	}
}

// TestExpireAtFromURL verifies expiry parsing from stream URLs. DouYu ws URLs
// carry a relative "expire" query (seconds) and tx CDN URLs carry an absolute
// hex "txTime" Unix timestamp; when both exist the earlier one wins.
func TestExpireAtFromURL(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		rawURL  string
		want    bool
		aboutIn time.Duration // expected ExpireAt distance from now (for want=true)
	}{
		{
			name:    "relative expire seconds",
			rawURL:  "wss://edge.example.com/a.b.c/live/stream.xs?wsAuth=abc&expire=300",
			want:    true,
			aboutIn: 300 * time.Second,
		},
		{
			name:    "absolute hex txTime",
			rawURL:  "https://cdn.example.com/live/stream.flv?txSecret=s&txTime=6ac40c47",
			want:    true,
			aboutIn: time.Unix(0x6ac40c47, 0).Sub(now),
		},
		{
			name: "both expire and far txTime picks expire",
			// txTime must be generated dynamically (far in the future) so the
			// test never rots once the hardcoded instant has passed.
			rawURL:  fmt.Sprintf("wss://edge.example.com/live/stream.xs?expire=300&txSecret=s&txTime=%x", time.Now().Add(6*time.Hour).Unix()),
			want:    true,
			aboutIn: 300 * time.Second,
		},
		{
			name:   "no expiry params",
			rawURL: "https://cdn.example.com/live/stream.flv?token=abc",
			want:   false,
		},
		{
			name:   "invalid expire value",
			rawURL: "wss://edge.example.com/live/stream.xs?expire=abc",
			want:   false,
		},
		{
			name:   "invalid txTime value",
			rawURL: "wss://edge.example.com/live/stream.xs?txTime=zzz",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parse url: %v", err)
			}
			got := expireAtFromURL(u)
			if !tt.want {
				if got != nil {
					t.Errorf("expireAtFromURL() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expireAtFromURL() = nil, want a time")
			}
			want := now.Add(tt.aboutIn)
			if diff := got.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
				t.Errorf("expireAtFromURL() = %v, want about %v (diff %s)", got, want, diff)
			}
		})
	}
}

// TestGetLinkRateStreamError verifies GetLink surfaces API errors instead of
// building a URL from empty fields (e.g. after the broadcaster goes offline).
func TestGetLinkRateStreamError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "api error with message",
			body: `{"error":104,"msg":"room is closed","data":{}}`,
		},
		{
			name: "missing rtmp fields",
			body: `{"error":0,"data":{"p2p":0}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &Link{rid: "123", rateStreamFn: func() (gjson.Result, error) {
				return gjson.Parse(tt.body), nil
			}}
			u, err := l.GetLink("flv")
			if err == nil {
				t.Fatalf("GetLink() = %v, want error for body %q", u, tt.body)
			}
		})
	}
}

// TestRateStreamErrorPlainText verifies plain-text API rejections (the API
// answers a bare "鉴权失败" string when the enc_data credentials expire)
// surface their actual reason instead of a misleading offline message.
func TestRateStreamErrorPlainText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "auth failure text", body: `"鉴权失败"`, want: "鉴权失败"},
		{name: "empty response", body: `""`, want: "empty rate stream response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateStreamError(gjson.Parse(tt.body)); got != tt.want {
				t.Errorf("rateStreamError(%s) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// TestGetLinkAuthFailureRetriesWithFreshEncData verifies that an expired
// enc_data (rejected with 鉴权失败 after ~10 minutes) triggers a credential
// refresh and a retry, so long-running streams keep refreshing their URL.
func TestGetLinkAuthFailureRetriesWithFreshEncData(t *testing.T) {
	goodRateStream := `{
		"error": 0,
		"data": {
			"p2p": 10,
			"rtmp_url": "https://dummy",
			"rtmp_live": "streamname.flv?wsAuth=abc&expire=300",
			"p2pMeta": {
				"dyxp2p_sug_egde": "edge.example.com",
				"dyxp2p_domain": "example.domain",
				"xp2p_txDelay": "5000",
				"xp2p_txSecret": "secret",
				"xp2p_txTime": "6ac40c47"
			}
		}
	}`
	var rateCalls, encRefreshes int
	l := &Link{
		rid:     "123",
		did:     "testdid",
		encData: `{"key":"oldkey","rand_str":"oldrand","enc_time":1,"enc_data":"oldenc"}`,
		rateStreamFn: func() (gjson.Result, error) {
			rateCalls++
			if rateCalls == 1 {
				// Expired credentials: the API answers a bare string.
				return gjson.Parse(`"鉴权失败"`), nil
			}
			return gjson.Parse(goodRateStream), nil
		},
		encryptDataFn: func() (string, error) {
			encRefreshes++
			return `{"key":"newkey","rand_str":"newrand","enc_time":0,"enc_data":"newenc"}`, nil
		},
	}

	u, err := l.GetLink("ws")
	if err != nil {
		t.Fatalf("GetLink() error: %v", err)
	}
	if u.Scheme != "wss" {
		t.Errorf("GetLink() scheme = %q, want wss", u.Scheme)
	}
	if rateCalls != 2 {
		t.Errorf("rate stream requested %d times, want 2 (original + retry)", rateCalls)
	}
	if encRefreshes != 1 {
		t.Errorf("enc data refreshed %d times, want 1", encRefreshes)
	}
}

// TestExtract_SetsExpireAtFromP2PURL runs a full p2p=10 (WebSocket) rate
// stream response through Extract and verifies the returned Result carries
// an ExpireAt derived from the expire query parameter.
func TestExtract_SetsExpireAtFromP2PURL(t *testing.T) {
	// txTime must be generated dynamically (far in the future) so the test
	// never rots once the hardcoded instant has passed.
	rateStream := fmt.Sprintf(`{
		"error": 0,
		"data": {
			"p2p": 10,
			"rtmp_url": "https://dummy",
			"rtmp_live": "streamname.flv?wsAuth=abc&expire=300",
			"p2pMeta": {
				"dyxp2p_sug_egde": "edge.example.com",
				"dyxp2p_domain": "example.domain",
				"xp2p_txDelay": "5000",
				"xp2p_txSecret": "secret",
				"xp2p_txTime": "%x"
			}
		}
	}`, time.Now().Add(6*time.Hour).Unix())
	l := &Link{rid: "123", rateStreamFn: func() (gjson.Result, error) {
		return gjson.Parse(rateStream), nil
	}}

	result, err := l.Extract("ws")
	if err != nil {
		t.Fatalf("Extract() error: %v", err)
	}
	u, err := url.Parse(result.URL)
	if err != nil {
		t.Fatalf("parse extracted url: %v", err)
	}
	if u.Scheme != "wss" {
		t.Errorf("extracted scheme = %q, want wss", u.Scheme)
	}
	if result.ExpireAt == nil {
		t.Fatal("Extract() returned nil ExpireAt, want expiry from expire=300")
	}
	if diff := time.Until(*result.ExpireAt) - 300*time.Second; diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("ExpireAt = %v, want about now+300s (diff %s)", result.ExpireAt, diff)
	}
}

func TestDouYu_CalcAuth_ZeroEncTime(t *testing.T) {
	// When enc_time is 0, the loop does not execute and authString
	// remains equal to rand_str.
	key := "mykey"
	randStr := "myrand"
	encTime := int64(0)
	rid := "99999"
	t10 := "9999999999"

	l := &Link{
		rid: rid,
		t10: t10,
		did: "testdid",
		encData: fmt.Sprintf(`{
			"key": "%s",
			"rand_str": "%s",
			"enc_time": %d,
			"expire_at": 0,
			"is_special": 0,
			"enc_data": ""
		}`, key, randStr, encTime),
	}

	auth, err := l.calculateAuth()
	if err != nil {
		t.Fatalf("calculateAuth() returned error: %v", err)
	}

	// With enc_time=0: authString stays as randStr, then one final MD5
	finalHash := md5.Sum([]byte(fmt.Sprintf("%s%s%s", randStr, key, fmt.Sprintf("%s%s", rid, t10))))
	expected := hex.EncodeToString(finalHash[:])

	if auth != expected {
		t.Errorf("calculateAuth() with enc_time=0 = %q, want %q", auth, expected)
	}
}
