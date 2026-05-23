package DouYu

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
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
