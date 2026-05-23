package HuYa

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

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

func TestHuYa_SupportedFormats(t *testing.T) {
	l := &Link{}
	formats := l.SupportedFormats()
	expected := []string{"flv", "hls"}
	if len(formats) != len(expected) {
		t.Fatalf("expected %d formats, got %d", len(expected), len(formats))
	}
	for i, f := range formats {
		if f != expected[i] {
			t.Errorf("format[%d]: expected %q, got %q", i, expected[i], f)
		}
	}
}

func TestHuYa_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "flv" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "flv")
	}
}

func TestHuYa_Registry(t *testing.T) {
	entry, ok := extractor.Registry["huya"]
	if !ok {
		t.Fatal("huya not registered in extractor.Registry")
	}
	if !entry.Mobile {
		t.Error("Mobile should be true, got false")
	}
	if entry.InitialError != 500 {
		t.Errorf("InitialError = %d, want 500", entry.InitialError)
	}
	if entry.Factory == nil {
		t.Error("Factory should not be nil")
	}
}

func TestHuYa_Extract_M3u8Normalization(t *testing.T) {
	// Set up a Link with eLiveStatus=2 and one flv stream entry.
	// When Extract("m3u8") normalizes to "hls", getLive("hls") will fail
	// with "no validate hls link found" because hls_stream_info is always
	// empty (hls processing is commented out in getLive). This error
	// proves the normalization occurred: without normalization, "m3u8"
	// would fall to the default case in getLive and return a flv URL.
	fmEncoded := base64.StdEncoding.EncodeToString([]byte("$0$1$2$3"))
	anticode := fmt.Sprintf("fm=%s&ctype=huya_live&t=100&wsTime=200", fmEncoded)

	roomJSON := fmt.Sprintf(`{
		"roomInfo": {
			"eLiveStatus": 2,
			"tLiveInfo": {
				"tLiveStreamInfo": {
					"vStreamInfo": {
						"value": [
							{
								"sFlvUrl": "https://flv.example.com/live",
								"sFlvAntiCode": "%s",
								"sStreamName": "teststream",
								"sFlvUrlSuffix": "flv"
							}
						]
					}
				}
			}
		}
	}`, anticode)

	l := &Link{
		rid:  "12345",
		uid:  "100001",
		uidi: 100001,
		uuid: "54321",
		res:  gjson.Parse(roomJSON),
	}

	_, err := l.Extract("m3u8")
	if err == nil {
		t.Fatal("expected error from Extract(\"m3u8\"), got nil")
	}
	// The "no validate hls link found" error proves m3u8 was normalized to hls.
	// Without normalization, "m3u8" would hit the default case in getLive
	// and return a flv URL instead of erroring.
	if !strings.Contains(err.Error(), "no validate hls link found") {
		t.Errorf("expected error containing 'no validate hls link found', got: %v", err)
	}
}
