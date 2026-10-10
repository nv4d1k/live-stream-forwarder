package sabr

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/ebml"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	m.Run()
}

// ---- UMP response builders ----

func umpFrame(typ int, payload []byte) []byte {
	b := appendUmpVarint(nil, uint64(typ))
	b = appendUmpVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func sabrMediaHeaderPart(headerID uint32, itag int32, startMs int64) []byte {
	var pb []byte
	pb = appendPBInt(pb, 1, uint64(headerID))
	pb = appendPBInt(pb, 3, uint64(uint32(itag)))
	pb = appendPBInt(pb, 11, uint64(startMs))
	return umpFrame(umpMediaHeader, pb)
}

func sabrMediaPart(headerID byte, data []byte) []byte {
	return umpFrame(umpMedia, append([]byte{headerID}, data...))
}

// webm builders: an EBML + Segment(open) + Info + Tracks + Cluster(open)
// document with 60fps VP9 frames, mirroring the SABR video payload.

func wElem(id uint64, payload []byte) []byte {
	out := ebml.AppendID(nil, id)
	out = ebml.AppendVint(out, uint64(len(payload)))
	return append(out, payload...)
}

func wElemOpen(id uint64) []byte {
	out := ebml.AppendID(nil, id)
	return append(out, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
}

func wSimpleBlock(track uint64, relMs int16, keyframe bool, data []byte) []byte {
	var p []byte
	p = ebml.AppendVint(p, track)
	p = append(p, byte(uint16(relMs)>>8), byte(relMs))
	f := byte(0)
	if keyframe {
		f = 0x80
	}
	p = append(p, f)
	return append(p, data...)
}

func webmDoc(clusterTC int64, relTcs []int16) []byte {
	var doc []byte
	doc = append(doc, wElem(0x1A45DFA3, wElem(0x4282, []byte("webm")))...)
	doc = append(doc, wElemOpen(0x18538067)...)
	doc = append(doc, wElem(0x1549A966, wElem(0x2AD7B1, []byte{0x0F, 0x42, 0x40}))...)
	var entry []byte
	entry = append(entry, wElem(0xD7, []byte{0x01})...) // TrackNumber 1
	entry = append(entry, wElem(0x83, []byte{0x01})...) // TrackType video
	entry = append(entry, wElem(0x86, []byte("V_VP9"))...)
	var dims []byte
	dims = append(dims, wElem(0xB0, []byte{0x0A, 0x00})...) // PixelWidth 2560
	dims = append(dims, wElem(0xBA, []byte{0x05, 0xA0})...) // PixelHeight 1440
	entry = append(entry, wElem(0xE0, dims)...)
	doc = append(doc, wElem(0x1654AE6B, wElem(0xAE, entry))...)
	doc = append(doc, wElemOpen(0x1F43B675)...)
	tc := make([]byte, 8)
	for i := 0; i < 8; i++ {
		tc[i] = byte(clusterTC >> uint(8*(7-i)))
	}
	doc = append(doc, wElem(0xE7, tc)...)
	for i, rel := range relTcs {
		doc = append(doc, wElem(0xA3, wSimpleBlock(1, rel, i == 0, []byte{byte('V'), byte(i)}))...)
	}
	return doc
}

// audioFMP4 builds one round's audio document: moov + moof + mdat.
func audioFMP4(baseTicks uint64, frames [][]byte) []byte {
	sizes := make([]uint32, len(frames))
	durs := make([]uint32, len(frames))
	var body []byte
	for i, f := range frames {
		sizes[i] = uint32(len(f))
		durs[i] = 1024
		body = append(body, f...)
	}
	var doc []byte
	doc = append(doc, aMoov(44100, 2, 44100, []byte{0x12, 0x10})...)
	doc = append(doc, aMoof(baseTicks, sizes, durs)...)
	doc = append(doc, aBox("mdat", body)...)
	return doc
}

// sabrRound builds one full SABR response round: protection status, both
// track headers, the audio fMP4 document and the video WebM document.
// Both tracks' content covers [baseMs, baseMs + len(tcs)*16] at 60fps and
// the audio at 23ms per frame.
func sabrRound(baseMs int64, tcs []int16) []byte {
	var out []byte
	out = append(out, umpFrame(umpStreamProtection, appendPBInt(nil, 1, 2))...)
	out = append(out, sabrMediaHeaderPart(0, 140, baseMs)...)
	out = append(out, sabrMediaHeaderPart(1, 308, baseMs)...)
	audioFrames := make([][]byte, len(tcs))
	for i := range audioFrames {
		audioFrames[i] = []byte{0xA0, byte(i)}
	}
	out = append(out, sabrMediaPart(0, audioFMP4(uint64(baseMs)*44100/1000, audioFrames))...)
	out = append(out, sabrMediaPart(1, webmDoc(baseMs, tcs))...)
	return out
}

// extractResultFor builds the extract result for the mock endpoint.
func extractResultFor(serverURL string) *stream.ExtractResult {
	return &stream.ExtractResult{
		URL: serverURL + "/sabr",
		Sabr: &extractor.SabrInfo{
			UstreamerConfig: []byte{0xDE, 0xAD},
			VideoITag:       308,
			AudioITag:       140,
		},
	}
}

// readFor reads everything the stream produces within d, then closes it.
func readFor(t *testing.T, r io.ReadCloser, d time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- b
	}()
	time.Sleep(d)
	r.Close()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not close within 5s")
		return nil
	}
}

// mkvBlocks walks the muxed output and returns the header bytes and every
// block: (track, absolute timecode, keyframe).
type mkvBlock struct {
	track    uint64
	tcMs     int64
	keyframe bool
}

func parseMKV(t *testing.T, data []byte) (header []byte, blocks []mkvBlock) {
	t.Helper()
	off := 0
	for off < len(data) {
		e, next, needMore, err := ebml.ScanElem(data, off)
		if err != nil {
			t.Fatalf("scan at %d: %v", off, err)
		}
		if needMore {
			break
		}
		switch e.ID {
		case 0x1A45DFA3, 0x18538067, 0x1549A966, 0x1654AE6B:
			header = append(header, data[off:next]...)
		case 0x1F43B675:
			var clusterTC int64
			p := e.DataOff
			for p < e.End {
				c, cnext, needMore, err := ebml.ScanElem(data, p)
				if err != nil || needMore {
					t.Fatalf("cluster scan: %v", err)
				}
				switch c.ID {
				case 0xE7:
					body := data[c.DataOff : c.DataOff+c.DataSize]
					var v int64
					for _, b := range body {
						v = v<<8 | int64(b)
					}
					clusterTC = v
				case 0xA3:
					payload := data[c.DataOff : c.DataOff+c.DataSize]
					track, rel, kf, _, ok := ebml.ScanSimpleBlock(payload)
					if !ok {
						t.Fatal("bad simple block")
					}
					blocks = append(blocks, mkvBlock{track, clusterTC + int64(rel), kf})
				}
				p = cnext
			}
		}
		off = next
	}
	return header, blocks
}

func TestSABRStream_Produce(t *testing.T) {
	roundIntervalMs.Store(50)
	defer roundIntervalMs.Store(1000)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("SABR requests must be POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-protobuf" {
			t.Errorf("content-type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) < 2 || body[0] != 120 {
			t.Errorf("request body is not a UMP type-120 frame: %x", body[:min(8, len(body))])
		}
		_, _ = w.Write(sabrRound(1000000, []int16{0, 17, 34}))
	}))
	defer ts.Close()

	s := NewSABRStream(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return extractResultFor(ts.URL), nil
	}, ts.Client())
	defer s.Close()

	data := readFor(t, s, 400*time.Millisecond)
	if len(data) == 0 {
		t.Fatal("no data")
	}

	header, blocks := parseMKV(t, data)
	if !bytes.Contains(header, []byte("V_VP9")) {
		t.Error("video track missing from MKV header")
	}
	if !bytes.Contains(header, []byte("A_AAC")) {
		t.Error("audio track missing from MKV header")
	}
	if !bytes.Contains(header, []byte("matroska")) {
		t.Error("DocType missing")
	}
	var video, audio int
	for _, b := range blocks {
		switch b.track {
		case 1:
			video++
			// The muxer normalizes to the first block's timecode: the
			// base round's frames land at 0/17/34.
			if b.tcMs != 0 && b.tcMs != 17 && b.tcMs != 34 {
				t.Errorf("video tc %d out of set", b.tcMs)
			}
		case 2:
			audio++
		}
	}
	if video < 3 {
		t.Errorf("video blocks = %d, want 3", video)
	}
	if audio < 3 {
		t.Errorf("audio blocks = %d, want 3", audio)
	}
}

func TestSABRStream_OverlapDedup(t *testing.T) {
	roundIntervalMs.Store(50)
	defer roundIntervalMs.Store(1000)

	// Three rounds with overlapping coverage: video frame timecodes are
	// baseMs + {0,17,34} ms, bases 1000 / 1001 / 1002 overlap heavily.
	bases := []int64{1000, 1017, 1034}
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)-1) % len(bases)
		_, _ = w.Write(sabrRound(bases[i], []int16{0, 17, 34}))
	}))
	defer ts.Close()

	s := NewSABRStream(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return extractResultFor(ts.URL), nil
	}, ts.Client())
	defer s.Close()

	data := readFor(t, s, 600*time.Millisecond)
	_, blocks := parseMKV(t, data)
	seenVideo := map[int64]bool{}
	seenAudio := map[int64]bool{}
	for _, b := range blocks {
		seen := seenVideo
		if b.track == 2 {
			seen = seenAudio
		}
		if seen[b.tcMs] {
			t.Errorf("track %d: duplicated block tc=%d — overlap dedup failed", b.track, b.tcMs)
		}
		seen[b.tcMs] = true
	}
	// All three rounds' content must be present exactly once. The muxer
	// normalizes to the first block (audio @1000 → base), so the union of
	// the three rounds' video frames is {0,17,34,51,68} on the output axis.
	for _, want := range []int64{0, 17, 34, 51, 68} {
		if !seenVideo[want] {
			t.Errorf("video tc %d missing (dedup must not drop new content)", want)
		}
	}
}

func TestSABRStream_EmptyRoundRetries(t *testing.T) {
	roundIntervalMs.Store(20)
	retryBackoffMs.Store(20)
	defer func() {
		roundIntervalMs.Store(1000)
		retryBackoffMs.Store(2000)
	}()

	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			// First round: attestation rejection — protection status only.
			_, _ = w.Write(umpFrame(umpStreamProtection, appendPBInt(appendPBInt(nil, 1, 3), 2, 10)))
			return
		}
		_, _ = w.Write(sabrRound(5000, []int16{0, 17}))
	}))
	defer ts.Close()

	s := NewSABRStream(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return extractResultFor(ts.URL), nil
	}, ts.Client())
	defer s.Close()

	data := readFor(t, s, 600*time.Millisecond)
	if len(data) == 0 {
		t.Fatal("stream never recovered from the empty round")
	}
	_, blocks := parseMKV(t, data)
	if len(blocks) == 0 {
		t.Fatal("no blocks after retry")
	}
}

func TestSABRStream_ExtractErrorRetries(t *testing.T) {
	roundIntervalMs.Store(20)
	retryBackoffMs.Store(20)
	defer func() {
		roundIntervalMs.Store(1000)
		retryBackoffMs.Store(2000)
	}()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sabrRound(7000, []int16{0}))
	}))
	defer ts.Close()

	var calls atomic.Int32
	s := NewSABRStream(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		if calls.Add(1) <= 2 {
			return nil, fmt.Errorf("innertube hiccup")
		}
		return extractResultFor(ts.URL), nil
	}, ts.Client())
	defer s.Close()

	data := readFor(t, s, 800*time.Millisecond)
	if len(data) == 0 {
		t.Fatal("stream never produced data after extract errors")
	}
}

func TestSABRStream_NonSabrResultCloses(t *testing.T) {
	s := NewSABRStream(func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: "http://example.com/x.flv"}, nil
	}, http.DefaultClient)
	err := s.Wait()
	if err == nil {
		t.Fatal("stream must close with an error when the result is not SABR")
	}
	_ = s.Close()
}
