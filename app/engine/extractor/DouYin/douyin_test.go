package DouYin

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
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

// enterResponseFixture mirrors the structure of a live
// /webcast/room/web/enter/ response (values redacted).
const enterResponseFixture = `{
  "data": {
    "data": [
      {
        "id_str": "7694264077454986020",
        "status": 2,
        "stream_url": {
          "flv_pull_url": {
            "FULL_HD1": "http://pull-flv-f11.douyinliving.com/thirdgame/stream_uhd.flv?expire=1792074710&sign=aaa",
            "HD1": "http://pull-flv-f11.douyinliving.com/thirdgame/stream_hd.flv?expire=1792074710&sign=bbb",
            "SD1": "http://pull-flv-f11.douyinliving.com/thirdgame/stream_ld.flv?expire=1792074710&sign=ccc",
            "SD2": "http://pull-flv-f11.douyinliving.com/thirdgame/stream_sd.flv?expire=1792074710&sign=ddd"
          },
          "hls_pull_url_map": {
            "FULL_HD1": "http://pull-hls-f11.douyinliving.com/thirdgame/stream_uhd.m3u8?expire=1792074710&sign=eee",
            "HD1": "http://pull-hls-f11.douyinliving.com/thirdgame/stream_hd.m3u8?expire=1792074710&sign=fff",
            "SD1": "http://pull-hls-f11.douyinliving.com/thirdgame/stream_ld.m3u8?expire=1792074710&sign=ggg",
            "SD2": "http://pull-hls-f11.douyinliving.com/thirdgame/stream_sd.m3u8?expire=1792074710&sign=hhh"
          },
          "default_resolution": "HD1"
        }
      }
    ]
  },
  "status_code": 0
}`

func TestDouYin_SupportedFormats(t *testing.T) {
	l := &Link{}
	formats := l.SupportedFormats()
	expected := []string{"flv", "m3u8"}
	if len(formats) != len(expected) {
		t.Fatalf("expected %d formats, got %d", len(expected), len(formats))
	}
	for i, f := range formats {
		if f != expected[i] {
			t.Errorf("format[%d]: expected %q, got %q", i, expected[i], f)
		}
	}
}

func TestDouYin_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "flv" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "flv")
	}
}

func TestDouYin_Registry(t *testing.T) {
	entry, ok := extractor.Registry["douyin"]
	if !ok {
		t.Fatal("douyin not registered in extractor.Registry")
	}
	if entry.UserAgent != "" {
		t.Errorf("UserAgent should be empty (default desktop UA), got %q", entry.UserAgent)
	}
	if entry.InitialError != 500 {
		t.Errorf("InitialError = %d, want 500", entry.InitialError)
	}
	if entry.Factory == nil {
		t.Error("Factory should not be nil")
	}
}

func TestPickStreamURL(t *testing.T) {
	room := gjson.Get(enterResponseFixture, "data.data.0")

	tests := []struct {
		name   string
		format string
		want   string
	}{
		{
			name:   "flv picks highest quality FULL_HD1",
			format: "flv",
			want:   "http://pull-flv-f11.douyinliving.com/thirdgame/stream_uhd.flv?expire=1792074710&sign=aaa",
		},
		{
			name:   "m3u8 picks highest quality FULL_HD1",
			format: "m3u8",
			want:   "http://pull-hls-f11.douyinliving.com/thirdgame/stream_uhd.m3u8?expire=1792074710&sign=eee",
		},
		{
			name:   "unknown format falls back to hls",
			format: "hls",
			want:   "http://pull-hls-f11.douyinliving.com/thirdgame/stream_uhd.m3u8?expire=1792074710&sign=eee",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickStreamURL(room, tt.format)
			if err != nil {
				t.Fatalf("pickStreamURL() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("pickStreamURL() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("unknown format with no hls urls errors", func(t *testing.T) {
		flvOnly := `{"stream_url":{"flv_pull_url":{"FULL_HD1":"http://example.com/x.flv"}}}`
		_, err := pickStreamURL(gjson.Parse(flvOnly), "m3u8")
		if err == nil {
			t.Error("pickStreamURL() should error when no url exists for the format")
		}
	})
}

func TestPickStreamURL_QualityFallback(t *testing.T) {
	// FULL_HD1 missing: the picker must degrade to the next available quality.
	room := `{"stream_url":{
		"flv_pull_url":{"HD1":"http://example.com/hd.flv","SD2":"http://example.com/sd.flv"},
		"hls_pull_url_map":{"SD2":"http://example.com/sd.m3u8"}
	}}`
	got, err := pickStreamURL(gjson.Parse(room), "flv")
	if err != nil {
		t.Fatalf("pickStreamURL() error = %v", err)
	}
	if got != "http://example.com/hd.flv" {
		t.Errorf("pickStreamURL() = %q, want HD1 fallback url", got)
	}

	got, err = pickStreamURL(gjson.Parse(room), "m3u8")
	if err != nil {
		t.Fatalf("pickStreamURL() error = %v", err)
	}
	if got != "http://example.com/sd.m3u8" {
		t.Errorf("pickStreamURL() = %q, want SD2 fallback url", got)
	}
}

func TestPickStreamURL_NoUrls(t *testing.T) {
	room := `{"stream_url":{"flv_pull_url":{},"hls_pull_url_map":{}}}`
	if _, err := pickStreamURL(gjson.Parse(room), "flv"); err == nil {
		t.Error("pickStreamURL() should error when all qualities are missing")
	}
}

func TestEnterAPIError(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  bool
		contains string
	}{
		{
			name:    "live room is ok",
			body:    enterResponseFixture,
			wantErr: false,
		},
		{
			name:     "api error code surfaces message",
			body:     `{"data":{"message":"该内容暂时无法无法查看"},"extra":{"now":1},"status_code":4001038}`,
			wantErr:  true,
			contains: "该内容暂时无法无法查看",
		},
		{
			name:     "api error code without message",
			body:     `{"data":{},"status_code":10011}`,
			wantErr:  true,
			contains: "10011",
		},
		{
			name:     "empty room list means offline or missing",
			body:     `{"data":{"data":[]},"status_code":0}`,
			wantErr:  true,
			contains: "offline",
		},
		{
			name:    "room without stream_url errors",
			body:    `{"data":{"data":[{"id_str":"1","status":2}]},"status_code":0}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := enterAPIError(gjson.Parse(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("enterAPIError() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.contains != "" && err != nil {
				if !strings.Contains(err.Error(), tt.contains) {
					t.Errorf("enterAPIError() = %q, want it to contain %q", err.Error(), tt.contains)
				}
			}
		})
	}
}

func TestExpireAtFromURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		want    time.Time
		wantNil bool
	}{
		{
			name:   "absolute unix expire param",
			rawURL: "http://pull-flv.douyinliving.com/x.flv?expire=1792074710&sign=aaa",
			want:   time.Unix(1792074710, 0),
		},
		{
			name:    "no expire param",
			rawURL:  "http://pull-flv.douyinliving.com/x.flv?sign=aaa",
			wantNil: true,
		},
		{
			name:    "non-numeric expire",
			rawURL:  "http://pull-flv.douyinliving.com/x.flv?expire=soon",
			wantNil: true,
		},
		{
			name:    "zero expire",
			rawURL:  "http://pull-flv.douyinliving.com/x.flv?expire=0",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("url.Parse() error = %v", err)
			}
			got := expireAtFromURL(u)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expireAtFromURL() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expireAtFromURL() = nil, want a time")
			}
			if !got.Equal(tt.want) {
				t.Errorf("expireAtFromURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnterResponseFixtureIsValidJSON(t *testing.T) {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(enterResponseFixture), &parsed); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
}
