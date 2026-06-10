package BiliBili

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
)

func TestMain(m *testing.M) {
	// global.Log is initialized in 0_test_init_test.go (runs before bilibili.go's init)
	os.Exit(m.Run())
}

func TestBiliBili_SupportedFormats(t *testing.T) {
	l := &Link{rid: "12345"}
	formats := l.SupportedFormats()
	if len(formats) != 2 {
		t.Fatalf("expected 2 formats, got %d", len(formats))
	}
	if formats[0] != "flv" {
		t.Errorf("expected formats[0] = flv, got %s", formats[0])
	}
	if formats[1] != "m3u8" {
		t.Errorf("expected formats[1] = m3u8, got %s", formats[1])
	}
}

func TestBiliBili_DefaultFormat(t *testing.T) {
	l := &Link{rid: "12345"}
	if df := l.DefaultFormat(); df != "flv" {
		t.Errorf("expected default format flv, got %s", df)
	}
}

func TestBiliBili_Extract_WithFormat(t *testing.T) {
	tests := []struct {
		name         string
		format       string
		expectSuffix string
		v2Response   *playInfoResponse
	}{
		{
			name:         "flv format",
			format:       "flv",
			expectSuffix: ".flv?key=abc",
			v2Response: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_stream",
						Formats: []formatItem{
							{
								FormatName: "flv",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/stream.flv",
										URLInfo: []urlItem{
											{Host: "https://cdn.example.com", Extra: "?key=abc"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
		},
		{
			name:         "m3u8 format",
			format:       "m3u8",
			expectSuffix: ".m3u8?key=abc",
			v2Response: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_hls",
						Formats: []formatItem{
							{
								FormatName: "fmp4",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/stream.m3u8",
										URLInfo: []urlItem{
											{Host: "https://cdn.example.com", Extra: "?key=abc"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/room/v1/Room/room_init":
					resp := roomInitResponse{Code: 0}
					resp.Data.RoomID = 99999
					resp.Data.LiveStatus = 1
					writeJSON(w, resp)

				case r.URL.Path == "/xlive/web-room/v2/index/getRoomPlayInfo":
					writeJSON(w, tt.v2Response)

				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			l := &Link{
				rid:    "12345",
				client: server.Client(),
			}
			l.client.Transport = &rewriteBaseTransport{
				base:    server.URL,
				wrapped: server.Client().Transport,
			}

			if err := l.resolveRoomID(); err != nil {
				t.Fatalf("resolveRoomID failed: %v", err)
			}

			result, err := l.Extract(tt.format)
			if err != nil {
				t.Fatalf("Extract(%q) failed: %v", tt.format, err)
			}
			if result == nil {
				t.Fatal("Extract returned nil result")
			}

			urlStr := result.URL
			if len(urlStr) < len(tt.expectSuffix) {
				t.Errorf("URL %q too short, expected suffix %q", urlStr, tt.expectSuffix)
			} else if urlStr[len(urlStr)-len(tt.expectSuffix):] != tt.expectSuffix {
				t.Errorf("expected URL ending with %q, got %q", tt.expectSuffix, urlStr)
			}

			if got := result.Headers.Get("Referer"); got != "https://live.bilibili.com" {
				t.Errorf("expected Referer https://live.bilibili.com, got %q", got)
			}
		})
	}
}

func TestBiliBili_SetCookie(t *testing.T) {
	l := &Link{rid: "12345", client: &http.Client{}}

	// Verify Link implements CookieSetter
	var _ extractor.CookieSetter = l

	// Verify SetCookie wraps transport
	originalTransport := l.client.Transport
	l.SetCookie("SESSDATA=test123")
	if l.cookie != "SESSDATA=test123" {
		t.Errorf("expected cookie to be set, got %q", l.cookie)
	}
	if l.client.Transport == originalTransport {
		t.Error("expected transport to be wrapped after SetCookie")
	}

	ct, ok := l.client.Transport.(*cookieTransport)
	if !ok {
		t.Fatal("expected transport to be cookieTransport")
	}
	if ct.cookie != "SESSDATA=test123" {
		t.Errorf("expected cookieTransport cookie to be SESSDATA=test123, got %q", ct.cookie)
	}
}

func TestBiliBili_SetCookie_Empty(t *testing.T) {
	l := &Link{rid: "12345", client: &http.Client{}}
	originalTransport := l.client.Transport
	l.SetCookie("")
	if l.cookie != "" {
		t.Errorf("expected empty cookie, got %q", l.cookie)
	}
	// Empty cookie should NOT wrap transport
	if l.client.Transport != originalTransport {
		t.Error("expected transport to be unchanged when cookie is empty")
	}
}

func TestCookieTransport_InjectsCookie(t *testing.T) {
	var receivedCookie string
	var receivedHost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookie = r.Header.Get("Cookie")
		receivedHost = r.Host
		w.WriteHeader(200)
	}))
	defer server.Close()

	ct := newCookieTransport(http.DefaultTransport, "SESSDATA=test; bili_jct=abc")
	client := &http.Client{Transport: &rewriteBaseTransport{
		base:    server.URL,
		wrapped: ct,
	}}

	// Simulate a request to api.live.bilibili.com
	req, _ := http.NewRequest("GET", "https://api.live.bilibili.com/test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if receivedCookie != "SESSDATA=test; bili_jct=abc" {
		t.Errorf("expected cookie to be injected, got %q", receivedCookie)
	}
	_ = receivedHost
}

func TestCookieTransport_SkipsNonBilibiliHost(t *testing.T) {
	var receivedCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer server.Close()

	ct := newCookieTransport(http.DefaultTransport, "SESSDATA=test")
	client := &http.Client{Transport: &rewriteBaseTransport{
		base:    server.URL,
		wrapped: ct,
	}}

	// Simulate a request to a CDN host
	req, _ := http.NewRequest("GET", "https://cdn.bilivideo.com/live/stream.flv", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if receivedCookie != "" {
		t.Errorf("expected no cookie for non-bilibili host, got %q", receivedCookie)
	}
}

// rewriteBaseTransport redirects all requests to the test server.
type rewriteBaseTransport struct {
	base    string
	wrapped http.RoundTripper
}

func (t *rewriteBaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newURL := t.base + req.URL.Path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequest(req.Method, newURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	newReq.Host = req.Host
	return t.wrapped.RoundTrip(newReq)
}

func TestSelectStreamURL(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		info        *playInfoResponse
		expectErr   bool
		errContains string
		expectURL   string
	}{
		{
			name:   "FLV format selection",
			format: "flv",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_stream",
						Formats: []formatItem{
							{
								FormatName: "flv",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/flv/base/",
										URLInfo: []urlItem{
											{Host: "https://cdn1.example.com", Extra: "?key=abc"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
			expectErr: false,
			expectURL: "https://cdn1.example.com/live/flv/base/?key=abc",
		},
		{
			name:   "HLS format with fmp4",
			format: "m3u8",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_hls",
						Formats: []formatItem{
							{
								FormatName: "fmp4",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/hls/fmp4/",
										URLInfo: []urlItem{
											{Host: "https://cdn2.example.com", Extra: "?key=def"},
										},
									},
								},
							},
							{
								FormatName: "ts",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/hls/ts/",
										URLInfo: []urlItem{
											{Host: "https://cdn3.example.com", Extra: "?key=ts1"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
			expectErr: false,
			expectURL: "https://cdn2.example.com/live/hls/fmp4/?key=def",
		},
		{
			name:   "HLS fallback to ts when fmp4 absent",
			format: "m3u8",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_hls",
						Formats: []formatItem{
							{
								FormatName: "ts",
								Codecs: []codecItem{
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/hls/ts/",
										URLInfo: []urlItem{
											{Host: "https://cdn4.example.com", Extra: "?key=tsfallback"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
			expectErr: false,
			expectURL: "https://cdn4.example.com/live/hls/ts/?key=tsfallback",
		},
		{
			name:   "Error when no matching protocol",
			format: "flv",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_hls",
						Formats: []formatItem{
							{FormatName: "fmp4", Codecs: []codecItem{}},
						},
					},
				}
				return resp
			}(),
			expectErr:   true,
			errContains: "no http_stream stream available",
		},
		{
			name:   "Error when no matching format",
			format: "flv",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_stream",
						Formats: []formatItem{
							{FormatName: "mp4", Codecs: []codecItem{}},
						},
					},
				}
				return resp
			}(),
			expectErr:   true,
			errContains: "no http_stream/flv stream available",
		},
		{
			name:   "Codec preference avc over hevc",
			format: "flv",
			info: func() *playInfoResponse {
				resp := &playInfoResponse{Code: 0}
				resp.Data.PlayURLInfo.PlayURL.Streams = []streamItem{
					{
						ProtocolName: "http_stream",
						Formats: []formatItem{
							{
								FormatName: "flv",
								Codecs: []codecItem{
									{
										CodecName: "hevc",
										CurrentQn: 10000,
										BaseURL:   "/live/flv/hevc/",
										URLInfo: []urlItem{
											{Host: "https://hevc.example.com", Extra: "?codec=hevc"},
										},
									},
									{
										CodecName: "avc",
										CurrentQn: 10000,
										BaseURL:   "/live/flv/avc/",
										URLInfo: []urlItem{
											{Host: "https://avc.example.com", Extra: "?codec=avc"},
										},
									},
								},
							},
						},
					},
				}
				return resp
			}(),
			expectErr: false,
			expectURL: "https://avc.example.com/live/flv/avc/?codec=avc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, err := selectStreamURL(tt.info, tt.format)
			if tt.expectErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url != tt.expectURL {
				t.Errorf("expected URL %q, got %q", tt.expectURL, url)
			}
		})
	}
}

func TestBiliBili_Registry(t *testing.T) {
	entry, ok := extractor.Registry["bilibili"]
	if !ok {
		t.Fatal("bilibili not found in extractor.Registry")
	}
	if entry.Mobile {
		t.Error("expected Mobile=false, got true")
	}
	if entry.InitialError != 500 {
		t.Errorf("expected InitialError=500, got %d", entry.InitialError)
	}
	if entry.Factory == nil {
		t.Error("expected Factory to be non-nil")
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
