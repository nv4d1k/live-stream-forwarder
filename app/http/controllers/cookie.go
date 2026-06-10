package controllers

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
)

func DecodeCookie(encoded string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
