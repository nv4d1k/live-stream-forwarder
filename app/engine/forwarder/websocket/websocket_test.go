package websocket

import (
	"errors"
	"os"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestIsWebSocketURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "ws:// URL",
			url:  "ws://example.com/live",
			want: true,
		},
		{
			name: "wss:// URL",
			url:  "wss://example.com/live",
			want: true,
		},
		{
			name: "http:// URL",
			url:  "http://example.com/live.flv",
			want: false,
		},
		{
			name: "https:// URL",
			url:  "https://example.com/live.m3u8",
			want: false,
		},
		{
			name: "invalid URL",
			url:  "://invalid",
			want: false,
		},
		{
			name: "empty string",
			url:  "",
			want: false,
		},
		{
			name: "ws with path and query",
			url:  "ws://example.com/live?token=abc",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWebSocketURL(tt.url)
			if got != tt.want {
				t.Errorf("isWebSocketURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestIsRetriableWS(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "error containing 403",
			err:  errors.New("websocket: close 403"),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "other error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "error with 403 in message",
			err:  errors.New("HTTP 403 Forbidden"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableWS(tt.err)
			if got != tt.want {
				t.Errorf("isRetriableWS(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
