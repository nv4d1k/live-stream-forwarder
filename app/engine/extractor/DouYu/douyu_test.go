package DouYu

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestCalcAuth(t *testing.T) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.Level(6))

	l, err := NewDouyuLink("414194", &url.URL{})
	if err != nil {
		t.Fatal(err)
	}
	enc := `{
        "key": "EdGyudfO3qGUxB3p5GYj384714",
        "rand_str": "Iv9nTUJjskabp6SD",
        "enc_time": 1,
        "expire_at": 1767550175,
        "is_special": 0,
        "cpp": {
            "danmu": {
                "key_ver": "1.0",
                "key": "MKy68tTD41YmvJZwgVKQ"
            },
            "heartbeat": {
                "key_ver": "1.1",
                "key": "68rdVozmeLwhgPoaGABH"
            }
        },
        "enc_data": "eyJhbGdfdmVyIjoxLCJrZXlfdmVyIjo4MzUsImtleSI6IkVkR3l1ZGZPM3FHVXhCM3A1R1lqMzg0NzE0IiwicmFuZF9zdHIiOiJJdjluVFVKanNrYWJwNlNEIiwiZW5jX3RpbWUiOjEsImV4cGlyZV9hdCI6MTc2NzU1MDE3NSwic2lnbiI6ImNmMWVmNTBkZjI3ZjZjZjRkYmFjNjMyZjIzNmIxZmY0Iiwib3AiOnsiZGlkIjoiYTNlODA1OGMzOTY0MTY5ZWExZTkwYzkxMDAwNTE3MDEiLCJpcCI6IjI0MDk6OGEwMDo4NDkxOmJhODA6OjEiLCJ0cyI6MTc2NzU0OTU3NSwidWEiOiJNb3ppbGxhLzUuMCAoV2luZG93cyBOVCAxMC4wOyBXaW42NDsgeDY0KSBBcHBsZVdlYktpdC81MzcuMzYgKEtIVE1MLCBsaWtlIEdlY2tvKSBDaHJvbWUvMTQzLjAuMC4wIFNhZmFyaS81MzcuMzYgRWRnLzE0My4wLjAuMCJ9LCJjcHAiOnsiZGFubXUiOnsia2V5X3ZlciI6IjEuMCIsImtleSI6Ik1LeTY4dFRENDFZbXZKWndnVktRIn0sImhlYXJ0YmVhdCI6eyJrZXlfdmVyIjoiMS4xIiwia2V5IjoiNjhyZFZvem1lTHdoZ1BvYUdBQkgifX19"
    }`

	l.encData = enc
	l.t10 = "1767549575"
	l.did = "a3e8058c3964169ea1e90c9100051701"
	fmt.Println(l.getRateStream())
}
