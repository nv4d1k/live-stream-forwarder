package controllers

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/ebml"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/fanout"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/sabr"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	m.Run()
}

// ---- fixture builders: one SABR round whose video track is a WebM
// document and whose audio track is a small fMP4, mirroring the real
// endpoint. (Minimal copies of the sabr/ebml test builders.) ----

// mp4Box builds an ISO-BMFF box.
func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// pbV encodes a standard protobuf varint (for UMP payloads).
func pbV(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

func pbInt(field int, v uint64) []byte {
	return append(pbV(uint64(field<<3)), pbV(v)...)
}

// umpV encodes a UMP frame varint (little-endian style).
func umpV(n uint64) []byte {
	switch {
	case n < 0x80:
		return []byte{byte(n)}
	case n < 0x4000:
		return []byte{byte(0x80 | (n & 0x3F)), byte(n >> 6)}
	default:
		return []byte{byte(0xC0 | (n & 0x1F)), byte(n >> 5), byte(n >> 13)}
	}
}

func umpPart(typ int, payload []byte) []byte {
	out := umpV(uint64(typ))
	out = append(out, umpV(uint64(len(payload)))...)
	return append(out, payload...)
}

// ebElem builds an EBML element with a known size.
func ebElem(id uint64, payload []byte) []byte {
	out := ebml.AppendID(nil, id)
	out = ebml.AppendVint(out, uint64(len(payload)))
	return append(out, payload...)
}

// ebElemOpen builds an EBML element with the 8-byte unknown size.
func ebElemOpen(id uint64) []byte {
	out := ebml.AppendID(nil, id)
	return append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
}

// sabxFixtureWebM builds the video document: VP9 track 2560x1440 with one
// keyframe at t=0.
func sabxFixtureWebM() []byte {
	var doc []byte
	doc = append(doc, ebElem(0x1A45DFA3, ebElem(0x4282, []byte("webm")))...)
	doc = append(doc, ebElemOpen(0x18538067)...)
	doc = append(doc, ebElem(0x1549A966, ebElem(0x2AD7B1, []byte{0x0F, 0x42, 0x40}))...)
	var entry []byte
	entry = append(entry, ebElem(0xD7, []byte{0x01})...)
	entry = append(entry, ebElem(0x83, []byte{0x01})...)
	entry = append(entry, ebElem(0x86, []byte("V_VP9"))...)
	dims := append(ebElem(0xB0, []byte{0x0A, 0x00}), ebElem(0xBA, []byte{0x05, 0xA0})...)
	entry = append(entry, ebElem(0xE0, dims)...)
	doc = append(doc, ebElem(0x1654AE6B, ebElem(0xAE, entry))...)
	doc = append(doc, ebElemOpen(0x1F43B675)...)
	doc = append(doc, ebElem(0xE7, []byte{0, 0, 0, 0, 0, 0, 0x1F, 0x40})...) // tc 8000ms
	// SimpleBlock: track 1, rel 0, keyframe, 2-byte frame.
	var blk []byte
	blk = ebml.AppendVint(blk, 1)
	blk = append(blk, 0x00, 0x00, 0x80, 0x33, 0xCC)
	doc = append(doc, ebElem(0xA3, blk)...)
	return doc
}

// sabxFixtureAudio builds the audio document: AAC 44.1k stereo CSD and one
// fragment of one frame.
func sabxFixtureAudio() []byte {
	// esds carrying a DecoderSpecificInfo of 2 bytes.
	dsi := []byte{0x05, 0x02, 0x12, 0x10}
	dcd := append([]byte{0x04, byte(len(dsi) + 13)}, make([]byte, 13)...)
	dcd = append(dcd, dsi...)
	es := append([]byte{0x03, byte(len(dcd) + 3)}, 0x00, 0x00, 0x00)
	es = append(es, dcd...)
	esds := mp4Box("esds", append([]byte{0, 0, 0, 0}, es...))

	mp4a := make([]byte, 28)
	binary.BigEndian.PutUint16(mp4a[16:18], 2)         // channels
	binary.BigEndian.PutUint32(mp4a[24:28], 44100<<16) // rate
	entry := append(mp4Box("mp4a", mp4a), esds...)
	stsd := mp4Box("stsd", append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, entry...))
	stbl := mp4Box("stbl", stsd)
	minf := mp4Box("minf", stbl)

	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 44100)
	var mdia []byte
	mdia = append(mdia, mp4Box("mdhd", mdhd)...)
	mdia = append(mdia, minf...)
	trak := mp4Box("trak", mp4Box("mdia", mdia))
	moov := mp4Box("moov", trak)

	tfhd := make([]byte, 8)
	tfdt := make([]byte, 12)
	tfdt[0] = 1
	binary.BigEndian.PutUint64(tfdt[4:12], 8000*44100/1000)
	// trun: flags duration+size, one sample of 2 bytes / 1024 ticks.
	trunBody := append([]byte{0, 0, 0, 1, 0, 0, 4, 0, 0, 0, 0, 2}, 0, 0)
	trun := mp4Box("trun", append([]byte{0, 0x00, 0x03, 0x00}, trunBody...))
	var trafChildren []byte
	trafChildren = append(trafChildren, mp4Box("tfhd", tfhd)...)
	trafChildren = append(trafChildren, mp4Box("tfdt", tfdt)...)
	trafChildren = append(trafChildren, trun...)
	moof := mp4Box("moof", mp4Box("traf", trafChildren))
	mdat := mp4Box("mdat", []byte{0xA0, 0xA1})

	var doc []byte
	doc = append(doc, mp4Box("ftyp", []byte("ftypdash"))...)
	doc = append(doc, moov...)
	doc = append(doc, moof...)
	doc = append(doc, mdat...)
	return doc
}

// sbxRoundResponse builds one full SABR round: protection status, both
// track media headers, the audio document and the video document.
func sbxRoundResponse() []byte {
	var out []byte
	out = append(out, umpPart(58, pbInt(1, 2))...) // StreamProtectionStatus: pending

	audioHeader := append(pbInt(1, 0), pbInt(3, 140)...) // headerId 0, itag 140
	out = append(out, umpPart(20, audioHeader)...)
	videoHeader := append(pbInt(1, 1), pbInt(3, 308)...) // headerId 1, itag 308
	out = append(out, umpPart(20, videoHeader)...)

	out = append(out, umpPart(21, append([]byte{0}, sabxFixtureAudio()...))...)
	out = append(out, umpPart(21, append([]byte{1}, sabxFixtureWebM()...))...)
	return out
}

// sabrExtractor returns a fixed SABR result pointing at the given endpoint.
type sabrExtractor struct{ endpoint string }

func (s *sabrExtractor) Extract(format string) (*extractor.Result, error) {
	return &extractor.Result{
		URL: s.endpoint + "?sabr=1",
		Sabr: &extractor.SabrInfo{
			UstreamerConfig: []byte{0x01},
			VideoITag:       308,
			AudioITag:       140,
		},
	}, nil
}
func (s *sabrExtractor) SupportedFormats() []string { return []string{"sabr"} }
func (s *sabrExtractor) DefaultFormat() string      { return "sabr" }

// TestForwarder_SABR_TwoClientsOneUpstream is the fan-out contract: two
// clients watching the same room (and quality) must be served by exactly one
// upstream SABR core, both receiving the same MKV prefix.
func TestForwarder_SABR_TwoClientsOneUpstream(t *testing.T) {
	var upstreamPosts atomic.Int32
	sabrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		upstreamPosts.Add(1)
		_, _ = w.Write(sbxRoundResponse())
	}))
	defer sabrServer.Close()

	// Isolate the global hub for this test.
	prevHub := sabr.DefaultHub
	sabr.DefaultHub = fanout.NewHub()
	defer func() { sabr.DefaultHub = prevHub }()

	extractor.Register("sabrtest", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return &sabrExtractor{endpoint: sabrServer.URL}, nil
		},
		InitialError: 500,
	})
	defer delete(extractor.Registry, "sabrtest")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/:platform/:room", Forwarder)

	// streamToClient holds the response open until the request context
	// ends, so every fetch carries a deadline that cuts it off after the
	// hub has piped the init and the first clusters.
	fetch := func(d time.Duration) (int, string, []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/sabrtest/fireplace?format=sabr&quality=1440p", nil).WithContext(ctx)
		router.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()
	}

	code1, ctype, body1 := fetch(500 * time.Millisecond)
	if code1 != 200 {
		t.Fatalf("client 1: status %d, want 200", code1)
	}
	if !strings.Contains(ctype, "matroska") {
		t.Errorf("content type = %q, want a matroska MIME", ctype)
	}
	if len(body1) == 0 {
		t.Fatal("client 1 got no data")
	}
	// The stream must open with the MKV header (EBML magic 1A45DFA3).
	if body1[0] != 0x1A || body1[1] != 0x45 || body1[2] != 0xDF || body1[3] != 0xA3 {
		t.Errorf("stream does not start with the EBML header: %x", body1[:4])
	}

	code2, _, body2 := fetch(500 * time.Millisecond)
	if code2 != 200 {
		t.Fatalf("client 2: status %d, want 200", code2)
	}
	if len(body2) == 0 {
		t.Fatal("client 2 got no data")
	}
	if n := sabr.DefaultHub.Len(); n != 1 {
		t.Errorf("hub holds %d shared streams for one room+quality, want exactly 1 (fan-out broken)", n)
	}

	// Both streams must start with the same MKV header: the second client
	// replays the buffered snapshot of the same core.
	initLen := 0
	off := 0
	for off+8 <= len(body1) {
		e, next, needMore, err := ebml.ScanElem(body1, off)
		if err != nil || needMore {
			break
		}
		if e.ID == 0x1F43B675 { // first Cluster ends the init
			initLen = off
			break
		}
		off = next
	}
	if initLen == 0 {
		t.Fatal("no cluster boundary found in stream 1")
	}
	if len(body2) < initLen {
		t.Fatalf("client 2 stream shorter than the init (%d < %d)", len(body2), initLen)
	}
	if string(body1[:initLen]) != string(body2[:initLen]) {
		t.Error("client 2 did not replay the same MKV header as client 1")
	}
}

// TestForwarder_SABR_QualityKeysSeparateUpstreams verifies that two
// qualities of the same room open independent upstream cores (the hub key
// includes the quality), while same-quality clients still share.
func TestForwarder_SABR_QualityKeysSeparateUpstreams(t *testing.T) {
	sabrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sbxRoundResponse())
	}))
	defer sabrServer.Close()

	prevHub := sabr.DefaultHub
	sabr.DefaultHub = fanout.NewHub()
	defer func() { sabr.DefaultHub = prevHub }()

	extractor.Register("sabrqual", extractor.RegistryEntry{
		Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
			return &sabrExtractor{endpoint: sabrServer.URL}, nil
		},
		InitialError: 500,
	})
	defer delete(extractor.Registry, "sabrqual")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/:platform/:room", Forwarder)

	fetch := func(target string) (int, []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		router.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	if code, _ := fetch("/sabrqual/room?format=sabr&quality=1440p"); code != 200 {
		t.Fatalf("1440p request: status %d", code)
	}
	if code, _ := fetch("/sabrqual/room?format=sabr&quality=2160p"); code != 200 {
		t.Fatalf("2160p request: status %d", code)
	}
	if n := sabr.DefaultHub.Len(); n != 2 {
		t.Errorf("hub holds %d streams, want 2 (one per quality)", n)
	}
	// A third client on the first quality must join the existing core.
	if code, _ := fetch("/sabrqual/room?format=sabr&quality=1440p"); code != 200 {
		t.Fatalf("second 1440p request: status %d", code)
	}
	if n := sabr.DefaultHub.Len(); n != 2 {
		t.Errorf("hub holds %d streams after same-quality rejoin, want 2", n)
	}
}
