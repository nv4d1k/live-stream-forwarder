package controllers

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"testing"
)

func encodeCookie(raw string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(raw))
	gz.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

func TestDecodeCookie(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"simple cookie", "SESSDATA=abc123; bili_jct=xyz", false},
		{"cookie with special chars", "SESSDATA=a%2Bb%3Dc; DedeUserID=12345", false},
		{"empty cookie", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeCookie(tt.raw)
			decoded, err := DecodeCookie(encoded)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if decoded != tt.raw {
				t.Errorf("expected %q, got %q", tt.raw, decoded)
			}
		})
	}
}

func TestDecodeCookie_InvalidBase64(t *testing.T) {
	_, err := DecodeCookie("!!!invalid!!!")
	if err == nil {
		t.Error("expected error for invalid base64, got nil")
	}
}

func TestDecodeCookie_InvalidGzip(t *testing.T) {
	encoded := base64.RawURLEncoding.EncodeToString([]byte("not gzip data"))
	_, err := DecodeCookie(encoded)
	if err == nil {
		t.Error("expected error for invalid gzip, got nil")
	}
}
