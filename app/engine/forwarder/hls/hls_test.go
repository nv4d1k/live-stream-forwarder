package hls

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/grafov/m3u8"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestPickHighestBandwidthVariant(t *testing.T) {
	// Parse a master playlist to create real Variant structs.
	masterPlaylist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720
mid.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
high.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480
mid2.m3u8
`
	playlist, listType, err := m3u8.DecodeFrom(strings.NewReader(masterPlaylist), true)
	if err != nil {
		t.Fatalf("failed to parse master playlist: %v", err)
	}
	if listType != m3u8.MASTER {
		t.Fatalf("expected MASTER playlist, got %d", listType)
	}
	masterPl := playlist.(*m3u8.MasterPlaylist)

	best := pickHighestBandwidthVariant(masterPl.Variants)
	if best.Bandwidth != 5000000 {
		t.Errorf("pickHighestBandwidthVariant returned bandwidth=%d, want 5000000", best.Bandwidth)
	}
	if best.URI != "high.m3u8" {
		t.Errorf("pickHighestBandwidthVariant returned URI=%s, want high.m3u8", best.URI)
	}

	// Single variant.
	singlePlaylist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
only.m3u8
`
	playlist2, _, err := m3u8.DecodeFrom(strings.NewReader(singlePlaylist), true)
	if err != nil {
		t.Fatalf("failed to parse single variant playlist: %v", err)
	}
	masterPl2 := playlist2.(*m3u8.MasterPlaylist)
	best = pickHighestBandwidthVariant(masterPl2.Variants)
	if best.Bandwidth != 800000 {
		t.Errorf("pickHighestBandwidthVariant with single variant returned bandwidth=%d, want 800000", best.Bandwidth)
	}

	// First variant is the best.
	firstBestPlaylist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=3840x2160
best.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360
low.m3u8
`
	playlist3, _, err := m3u8.DecodeFrom(strings.NewReader(firstBestPlaylist), true)
	if err != nil {
		t.Fatalf("failed to parse first-best playlist: %v", err)
	}
	masterPl3 := playlist3.(*m3u8.MasterPlaylist)
	best = pickHighestBandwidthVariant(masterPl3.Variants)
	if best.URI != "best.m3u8" {
		t.Errorf("pickHighestBandwidthVariant returned URI=%s, want best.m3u8", best.URI)
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		refURL   string
		expected string
	}{
		{
			name:     "relative path",
			baseURL:  "https://example.com/live/master.m3u8",
			refURL:   "stream0.m3u8",
			expected: "https://example.com/live/stream0.m3u8",
		},
		{
			name:     "absolute URL",
			baseURL:  "https://example.com/live/master.m3u8",
			refURL:   "https://cdn.example.com/stream.m3u8",
			expected: "https://cdn.example.com/stream.m3u8",
		},
		{
			name:     "path with subdirectory",
			baseURL:  "https://example.com/live/hd/master.m3u8",
			refURL:   "segment.ts",
			expected: "https://example.com/live/hd/segment.ts",
		},
		{
			name:     "relative with dot",
			baseURL:  "https://example.com/live/master.m3u8",
			refURL:   "./stream.m3u8",
			expected: "https://example.com/live/stream.m3u8",
		},
		{
			name:     "invalid base URL returns refURL as-is",
			baseURL:  "://invalid",
			refURL:   "https://cdn.example.com/stream.m3u8",
			expected: "https://cdn.example.com/stream.m3u8",
		},
		{
			name:     "invalid ref URL returns refURL as-is",
			baseURL:  "https://example.com/live/master.m3u8",
			refURL:   "://invalid",
			expected: "://invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveURL(tt.baseURL, tt.refURL)
			if got != tt.expected {
				t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.baseURL, tt.refURL, got, tt.expected)
			}
		})
	}
}

func TestIsRetriableHLS(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "error containing 403",
			err:  errors.New("get m3u8 err got: 403 Forbidden"),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "other error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "error with 403 in status text",
			err:  errors.New("fetch segment err got: HTTP 403"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableHLS(tt.err)
			if got != tt.want {
				t.Errorf("isRetriableHLS(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
