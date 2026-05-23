package DouYin

import (
	"encoding/json"
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

func TestDouYin_ExtractJSON(t *testing.T) {
	l := &Link{}

	tests := []struct {
		name   string
		input  string
		wantOK bool
	}{
		{
			name:   "valid input with common field",
			input:  `self.__pace_f.push([1,"{\"common\":{\"aid\":\"test\"},\"data\":{\"origin\":{}}}"])`,
			wantOK: true,
		},
		{
			name:   "invalid input without pace_f pattern",
			input:  `some random text without the expected pattern`,
			wantOK: false,
		},
		{
			name:   "missing common field",
			input:  `self.__pace_f.push([1,"{\"data\":{\"origin\":{}}}"])`,
			wantOK: false,
		},
		{
			name:   "malformed JSON inside pace_f",
			input:  `self.__pace_f.push([1,"not valid json"])`,
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  ``,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := l.extractJSON(tt.input)
			if ok != tt.wantOK {
				t.Errorf("extractJSON() ok = %v, want %v", ok, tt.wantOK)
				return
			}
			if tt.wantOK {
				var parsed map[string]json.RawMessage
				if err := json.Unmarshal([]byte(result), &parsed); err != nil {
					t.Errorf("result is not valid JSON: %v", err)
				}
				if _, hasCommon := parsed["common"]; !hasCommon {
					t.Error("result should contain 'common' field")
				}
			}
		})
	}
}
