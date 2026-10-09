package YouTube

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	libm3u8 "github.com/grafov/m3u8"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

// testHLSURL mimics the shape of a real googlevideo HLS manifest URL,
// including the /expire/<unix>/ path segment parsed into ExpireAt.
const testHLSURL = "https://manifest.googlevideo.com/api/manifest/hls_variant/expire/1791572998/ei/test/ip/2401:2660:1/id/WWTcu33u00A.1/itag/0/file/index.m3u8"

// playerJSON builds a minimal innertube player response body.
func playerJSON(playability, reason string, isLive bool, hlsURL string) string {
	return fmt.Sprintf(`{"videoDetails":{"videoId":"WWTcu33u00A","title":"Test Live","isLive":%t},"playabilityStatus":{"status":%q,"reason":%q},"streamingData":{"hlsManifestUrl":%q}}`,
		isLive, playability, reason, hlsURL)
}

// newPlayerServer spins up a mock innertube player API that answers every
// request with the given status and body, recording requests for assertions.
func newPlayerServer(t *testing.T, status int, body string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var requests []*http.Request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rb, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(rb)))
		requests = append(requests, r)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &requests
}

func TestYouTube_SupportedFormats(t *testing.T) {
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

func TestYouTube_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "m3u8" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "m3u8")
	}
}

func TestYouTube_Registry(t *testing.T) {
	entry, ok := extractor.Registry["youtube"]
	if !ok {
		t.Fatal("youtube not registered in extractor.Registry")
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

func TestYouTube_NewYouTubeLink_ValidID(t *testing.T) {
	l, err := NewYouTubeLink("WWTcu33u00A", nil)
	if err != nil {
		t.Fatalf("NewYouTubeLink() returned error: %v", err)
	}
	if l.rid != "WWTcu33u00A" {
		t.Errorf("rid = %q, want %q", l.rid, "WWTcu33u00A")
	}
	if l.apiBase != defaultAPIBase {
		t.Errorf("apiBase = %q, want %q", l.apiBase, defaultAPIBase)
	}
	if l.client == nil {
		t.Error("client should not be nil")
	}
}

func TestYouTube_NewYouTubeLink_InvalidID(t *testing.T) {
	for _, rid := range []string{"", "short", "way-too-long-video-id", "has space!", "汉字直播间"} {
		if _, err := NewYouTubeLink(rid, nil); err == nil {
			t.Errorf("NewYouTubeLink(%q) should return error for invalid video ID", rid)
		}
	}
}

func TestYouTube_NewYouTubeLink_WithProxy(t *testing.T) {
	proxyURL, _ := url.Parse("socks5://proxy:1080")
	l, err := NewYouTubeLink("WWTcu33u00A", proxyURL)
	if err != nil {
		t.Fatalf("NewYouTubeLink() returned error: %v", err)
	}
	if l.client == nil {
		t.Error("client should not be nil")
	}
}

func TestYouTube_Extract_Live(t *testing.T) {
	ts, reqs := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, testHLSURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	result, err := l.Extract("m3u8")
	if err != nil {
		t.Fatalf("Extract() returned error: %v", err)
	}
	if result.URL != testHLSURL {
		t.Errorf("URL = %q, want %q", result.URL, testHLSURL)
	}
	if len(*reqs) != 1 {
		t.Fatalf("expected 1 API request, got %d", len(*reqs))
	}

	// ExpireAt parsed from the /expire/<unix>/ path segment.
	if result.ExpireAt == nil {
		t.Fatal("ExpireAt should not be nil")
	}
	expectedExpire := time.Unix(1791572998, 0)
	if result.ExpireAt.Unix() != expectedExpire.Unix() {
		t.Errorf("ExpireAt = %v, want %v", result.ExpireAt, expectedExpire)
	}

	// No quality set: no variant selector, forwarder picks highest bandwidth.
	if result.VariantSelector != nil {
		t.Error("VariantSelector should be nil without SetQuality")
	}
}

func TestYouTube_Extract_RequestShape(t *testing.T) {
	ts, reqs := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, testHLSURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	if _, err := l.Extract("m3u8"); err != nil {
		t.Fatalf("Extract() returned error: %v", err)
	}

	req := (*reqs)[0]
	if req.Method != "POST" {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL.Path != innertubePlayerPath {
		t.Errorf("path = %q, want %q", req.URL.Path, innertubePlayerPath)
	}
	if req.URL.Query().Get("key") != innertubeAPIKey {
		t.Errorf("api key query param missing or wrong: %q", req.URL.Query().Get("key"))
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("User-Agent"); got != androidUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, androidUserAgent)
	}
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), `"clientName":"ANDROID"`) {
		t.Errorf("request body should carry ANDROID clientName, got: %s", string(body))
	}
	if !strings.Contains(string(body), `"videoId":"WWTcu33u00A"`) {
		t.Errorf("request body should carry the video ID, got: %s", string(body))
	}
}

func TestYouTube_Extract_LoginRequired(t *testing.T) {
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("LOGIN_REQUIRED", "Sign in to confirm you're not a bot", true, ""))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error for LOGIN_REQUIRED")
	}
	if !strings.Contains(err.Error(), "Sign in to confirm") {
		t.Errorf("error should surface playability reason, got: %v", err)
	}
}

func TestYouTube_Extract_NotLive(t *testing.T) {
	// A VOD or finished stream: playability OK but isLive false, even with an
	// HLS manifest present.
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", false, testHLSURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error when video is not live")
	}
}

func TestYouTube_Extract_NoHLSManifest(t *testing.T) {
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, ""))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error when no HLS manifest URL is present")
	}
}

func TestYouTube_Extract_APIError(t *testing.T) {
	ts, _ := newPlayerServer(t, http.StatusInternalServerError, "internal error")

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("Extract() should return error for non-200 API response")
	}
}

func TestYouTube_Extract_NoExpireSegment(t *testing.T) {
	noExpireURL := "https://manifest.googlevideo.com/api/manifest/hls_variant/itag/0/file/index.m3u8"
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, noExpireURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	result, err := l.Extract("m3u8")
	if err != nil {
		t.Fatalf("Extract() should succeed without expire segment: %v", err)
	}
	if result.ExpireAt != nil {
		t.Errorf("ExpireAt should be nil when URL has no expire segment, got %v", result.ExpireAt)
	}
}

func TestYouTube_Extract_WithQuality(t *testing.T) {
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, testHLSURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	l.SetQuality("720p")
	result, err := l.Extract("m3u8")
	if err != nil {
		t.Fatalf("Extract() returned error: %v", err)
	}
	sel, ok := result.VariantSelector.(func([]*libm3u8.Variant) *libm3u8.Variant)
	if !ok {
		t.Fatalf("VariantSelector should be set after SetQuality, got %T", result.VariantSelector)
	}
	got := sel(testVariants())
	if got == nil || got.URI != "v720.m3u8" {
		t.Errorf("selector picked %v, want v720.m3u8", got)
	}
}

func TestYouTube_Extract_InvalidQuality(t *testing.T) {
	ts, _ := newPlayerServer(t, http.StatusOK, playerJSON("OK", "", true, testHLSURL))

	l := &Link{rid: "WWTcu33u00A", client: ts.Client(), apiBase: ts.URL}
	l.SetQuality("garbage")
	result, err := l.Extract("m3u8")
	if err != nil {
		t.Fatalf("Extract() returned error: %v", err)
	}
	if result.VariantSelector != nil {
		t.Error("VariantSelector should be nil for unparseable quality")
	}
}

func TestParseExpireAtFromManifestURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int64
		wantErr bool
	}{
		{"valid expire", "https://manifest.googlevideo.com/api/manifest/hls_variant/expire/1791572998/ei/x/file/index.m3u8", 1791572998, false},
		{"expire at end", "https://host/api/expire/42", 42, false},
		{"no expire segment", "https://manifest.googlevideo.com/api/manifest/hls_variant/itag/0/file/index.m3u8", 0, true},
		{"non numeric expire", "https://host/api/expire/soon/file/index.m3u8", 0, true},
		{"unparseable url", "://bad url", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expireAtFromManifestURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Unix() != tc.want {
				t.Errorf("expire = %d, want %d", got.Unix(), tc.want)
			}
		})
	}
}

func TestParseQualityHeight(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"720p", 720},
		{"720", 720},
		{"1080P", 1080},
		{" 480p ", 480},
		{"garbage", 0},
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"720x1280", 0},
	}
	for _, tc := range tests {
		if got := parseQualityHeight(tc.input); got != tc.want {
			t.Errorf("parseQualityHeight(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// testVariants mirrors the six muxed TS variants a YouTube live master
// playlist serves (itag 91/92/93/94/300/301).
func testVariants() []*libm3u8.Variant {
	return []*libm3u8.Variant{
		{URI: "v144.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 269034, Resolution: "256x144"}},
		{URI: "v240.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 507418, Resolution: "426x240"}},
		{URI: "v360.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 962461, Resolution: "640x360"}},
		{URI: "v480.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 1282517, Resolution: "854x480"}},
		{URI: "v720.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 2922155, Resolution: "1280x720"}},
		{URI: "v1080.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 5552610, Resolution: "1920x1080"}},
	}
}

func TestVariantSelectorForHeight(t *testing.T) {
	tests := []struct {
		name      string
		target    int
		variants  []*libm3u8.Variant
		wantURI   string
		wantPanic bool
	}{
		{"exact match", 720, testVariants(), "v720.m3u8", false},
		{"fall down to closest", 500, testVariants(), "v480.m3u8", false},
		{"all taller picks shortest", 90, testVariants(), "v144.m3u8", false},
		{"target above all picks tallest", 4320, testVariants(), "v1080.m3u8", false},
		{"no resolutions falls back to bandwidth", 720, []*libm3u8.Variant{
			{URI: "a.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 100}},
			{URI: "b.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 900}},
		}, "b.m3u8", false},
		{"mixed resolutions skips unparsable", 720, []*libm3u8.Variant{
			{URI: "bad.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 9999}},
			{URI: "v720.m3u8", VariantParams: libm3u8.VariantParams{Bandwidth: 2922155, Resolution: "1280x720"}},
		}, "v720.m3u8", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel := variantSelectorForHeight(tc.target)
			if sel == nil {
				t.Fatal("variantSelectorForHeight returned nil selector")
			}
			got := sel(tc.variants)
			if got == nil {
				t.Fatal("selector must never return nil for non-empty variants")
			}
			if got.URI != tc.wantURI {
				t.Errorf("selected %q, want %q", got.URI, tc.wantURI)
			}
		})
	}
}

func TestYouTube_SetQuality(t *testing.T) {
	l := &Link{rid: "WWTcu33u00A"}
	if l.quality != "" {
		t.Errorf("quality should start empty, got %q", l.quality)
	}
	l.SetQuality("480p")
	if l.quality != "480p" {
		t.Errorf("quality = %q, want %q", l.quality, "480p")
	}
}
