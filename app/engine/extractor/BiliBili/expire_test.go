package BiliBili

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBiliBili_expireAtFromURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantNil bool
		wantAt  time.Time
	}{
		{
			name:    "absolute unix timestamp in expires",
			rawURL:  "https://cdn.example.com/live/stream.flv?expires=1791386778&qn=250",
			wantNil: false,
			wantAt:  time.Unix(1791386778, 0),
		},
		{
			name:    "no expires param",
			rawURL:  "https://cdn.example.com/live/stream.flv?qn=250",
			wantNil: true,
		},
		{
			name:    "unparsable expires value",
			rawURL:  "https://cdn.example.com/live/stream.flv?expires=abc",
			wantNil: true,
		},
		{
			name:    "zero expires value",
			rawURL:  "https://cdn.example.com/live/stream.flv?expires=0",
			wantNil: true,
		},
		{
			name:    "negative expires value",
			rawURL:  "https://cdn.example.com/live/stream.flv?expires=-100",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("failed to parse URL: %v", err)
			}
			got := expireAtFromURL(u)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expireAtFromURL(%q) = %v, want nil", tt.rawURL, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expireAtFromURL(%q) = nil, want %v", tt.rawURL, tt.wantAt)
			}
			if !got.Equal(tt.wantAt) {
				t.Errorf("expireAtFromURL(%q) = %v, want %v", tt.rawURL, got, tt.wantAt)
			}
		})
	}
}

// TestBiliBili_Extract_SetsExpireAt verifies that Extract parses the
// expires parameter of the stream URL into Result.ExpireAt so the
// forwarders can refresh the URL proactively before it expires.
func TestBiliBili_Extract_SetsExpireAt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/room/v1/Room/room_init":
			resp := roomInitResponse{Code: 0}
			resp.Data.RoomID = 99999
			resp.Data.LiveStatus = 1
			writeJSON(w, resp)

		case r.URL.Path == "/xlive/web-room/v2/index/getRoomPlayInfo":
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
									CurrentQn: 250,
									BaseURL:   "/live/stream.flv",
									URLInfo: []urlItem{
										{Host: "https://cdn.example.com", Extra: "?expires=1791386778&key=abc"},
									},
								},
							},
						},
					},
				},
			}
			writeJSON(w, resp)

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

	result, err := l.Extract("flv")
	if err != nil {
		t.Fatalf("Extract(flv) failed: %v", err)
	}
	if result.ExpireAt == nil {
		t.Fatal("expected ExpireAt to be set from expires param, got nil")
	}
	if want := time.Unix(1791386778, 0); !result.ExpireAt.Equal(want) {
		t.Errorf("ExpireAt = %v, want %v", result.ExpireAt, want)
	}
}
