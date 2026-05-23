package Twitch

import (
	"net/url"
	"os"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestTwitch_SupportedFormats(t *testing.T) {
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

func TestTwitch_DefaultFormat(t *testing.T) {
	l := &Link{}
	if got := l.DefaultFormat(); got != "m3u8" {
		t.Errorf("DefaultFormat() = %q, want %q", got, "m3u8")
	}
}

func TestTwitch_Registry(t *testing.T) {
	entry, ok := extractor.Registry["twitch"]
	if !ok {
		t.Fatal("twitch not registered in extractor.Registry")
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

func TestTwitch_GetLink(t *testing.T) {
	l := &Link{
		rid:   "testchannel",
		sig:   "test_sig_value",
		token: "test_token_value",
	}

	u, err := l.GetLink("m3u8")
	if err != nil {
		t.Fatalf("GetLink() returned error: %v", err)
	}

	// Verify scheme and host
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want %q", u.Scheme, "https")
	}
	if u.Host != "usher.ttvnw.net" {
		t.Errorf("host = %q, want %q", u.Host, "usher.ttvnw.net")
	}

	// Verify path contains the room ID and .m3u8 suffix
	expectedPath := "/api/channel/hls/testchannel.m3u8"
	if u.Path != expectedPath {
		t.Errorf("path = %q, want %q", u.Path, expectedPath)
	}

	// Verify required query parameters
	query := u.Query()

	if got := query.Get("sig"); got != "test_sig_value" {
		t.Errorf("sig param = %q, want %q", got, "test_sig_value")
	}
	if got := query.Get("token"); got != "test_token_value" {
		t.Errorf("token param = %q, want %q", got, "test_token_value")
	}

	// Verify other required query parameters are present
	requiredParams := []string{
		"allow_source",
		"allow_audio_only",
		"allow_spectre",
		"fast_bread",
		"p",
		"play_session_id",
		"player_backend",
		"playlist_include_framerate",
		"reassignments_supported",
		"cdm",
		"player_version",
	}
	for _, param := range requiredParams {
		if !query.Has(param) {
			t.Errorf("missing required query parameter: %q", param)
		}
	}

	// Verify specific fixed parameter values
	if got := query.Get("allow_source"); got != "true" {
		t.Errorf("allow_source = %q, want %q", got, "true")
	}
	if got := query.Get("player_backend"); got != "mediaplayer" {
		t.Errorf("player_backend = %q, want %q", got, "mediaplayer")
	}
	if got := query.Get("cdm"); got != "wv" {
		t.Errorf("cdm = %q, want %q", got, "wv")
	}
	if got := query.Get("player_version"); got != "1.30.0" {
		t.Errorf("player_version = %q, want %q", got, "1.30.0")
	}

	// Verify the URL can round-trip through url.Parse
	reparsed, err := url.Parse(u.String())
	if err != nil {
		t.Errorf("GetLink() result cannot be reparsed: %v", err)
	}
	if reparsed.Host != u.Host {
		t.Errorf("reparsed host = %q, want %q", reparsed.Host, u.Host)
	}
}

func TestTwitch_GetLink_DifferentChannels(t *testing.T) {
	channels := []struct {
		rid string
	}{
		{rid: "shroud"},
		{rid: "xqc"},
		{rid: "pokimane"},
	}

	for _, tc := range channels {
		t.Run(tc.rid, func(t *testing.T) {
			l := &Link{
				rid:   tc.rid,
				sig:   "sig_" + tc.rid,
				token: "token_" + tc.rid,
			}
			u, err := l.GetLink("m3u8")
			if err != nil {
				t.Fatalf("GetLink() error: %v", err)
			}
			if u.Path != "/api/channel/hls/"+tc.rid+".m3u8" {
				t.Errorf("path = %q, want to contain channel %q", u.Path, tc.rid)
			}
			if u.Query().Get("sig") != "sig_"+tc.rid {
				t.Errorf("sig does not match for channel %q", tc.rid)
			}
		})
	}
}
