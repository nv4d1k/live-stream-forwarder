package mkv

import (
	"bytes"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/ebml"
)

// clusterBlocks walks muxed output and returns the clusters with their
// blocks: (track, timecodeMs, keyframe, data).
type blockOut struct {
	track    uint64
	tcMs     int64
	keyframe bool
	data     []byte
}

func parseMuxed(t *testing.T, out []byte) (header []byte, clusters [][]blockOut) {
	t.Helper()
	off := 0
	var segOpenSeen bool
	var init []byte
	for off < len(out) {
		e, next, needMore, err := ebml.ScanElem(out, off)
		if err != nil {
			t.Fatalf("scan at %d: %v", off, err)
		}
		if needMore {
			t.Fatalf("needMore at %d with %d bytes left", off, len(out)-off)
		}
		switch e.ID {
		case 0x1A45DFA3, 0x1549A966, 0x1654AE6B:
			init = append(init, out[off:next]...)
		case 0x18538067:
			segOpenSeen = true
			init = append(init, out[off:next]...)
		case 0x1F43B675:
			var clusterTC int64
			var blocks []blockOut
			p := e.DataOff
			for p < e.End {
				c, cnext, needMore, err := ebml.ScanElem(out, p)
				if err != nil || needMore {
					t.Fatalf("cluster child scan: %v", err)
				}
				switch c.ID {
				case 0xE7:
					body := out[c.DataOff : c.DataOff+c.DataSize]
					var v int64
					for _, b := range body {
						v = v<<8 | int64(b)
					}
					clusterTC = v
				case 0xA3:
					payload := out[c.DataOff : c.DataOff+c.DataSize]
					track, rel, kf, data, ok := ebml.ScanSimpleBlock(payload)
					if !ok {
						t.Fatalf("bad simple block")
					}
					blocks = append(blocks, blockOut{track, clusterTC + int64(rel), kf, data})
				}
				p = cnext
			}
			clusters = append(clusters, blocks)
		}
		off = next
	}
	if !segOpenSeen {
		t.Fatal("no Segment in output")
	}
	return init, clusters
}

func TestMuxerHeader(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(2560, 1440)
	m.SetAudio(44100, 2, []byte{0x12, 0x10})
	if err := m.WriteHeader(); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Reverse-parse the header: EBML + open Segment + Info + Tracks with
	// both entries.
	header, clusters := parseMuxed(t, buf.Bytes())
	if len(clusters) != 0 {
		t.Fatal("header write must not emit clusters")
	}
	if !bytes.Contains(header, []byte("matroska")) {
		t.Error("DocType missing")
	}
	if !bytes.Contains(header, []byte("V_VP9")) {
		t.Error("video codec ID missing")
	}
	if !bytes.Contains(header, []byte("A_AAC")) {
		t.Error("audio codec ID missing")
	}
	// CSD lands in CodecPrivate.
	off := bytes.Index(header, []byte("V_VP9"))
	cp := bytes.Index(header, []byte{0x12, 0x10})
	if off < 0 || cp < 0 || cp < off {
		t.Errorf("codec private not found after codec IDs (off=%d cp=%d)", off, cp)
	}
}

func TestMuxerFeedFlushInterleave(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(2560, 1440)
	m.SetAudio(44100, 2, nil)
	if err := m.WriteHeader(); err != nil {
		t.Fatal(err)
	}

	// Frames arrive per track in ascending order (the demuxers guarantee
	// that); the two tracks interleave arbitrarily. The muxer output must
	// be time-ordered across tracks.
	for _, f := range []struct {
		track int
		tc    int64
		kf    bool
		data  string
	}{
		{TrackVideo, 0, true, "v0"},
		{TrackAudio, 0, false, "a0"},
		{TrackVideo, 16, false, "v1"},
		{TrackAudio, 23, false, "a1"},
		{TrackVideo, 33, false, "v2"},
	} {
		if err := m.Feed(f.track, f.tc, f.kf, []byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	_, clusters := parseMuxed(t, buf.Bytes())
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(clusters))
	}
	blocks := clusters[0]
	if len(blocks) != 5 {
		t.Fatalf("blocks = %d, want 5", len(blocks))
	}
	// Time-ordered interleave regardless of feed order.
	want := []struct {
		track uint64
		tc    int64
	}{
		{1, 0}, {2, 0}, {1, 16}, {2, 23}, {1, 33},
	}
	for i, b := range blocks {
		if b.track != want[i].track || b.tcMs != want[i].tc {
			t.Errorf("block %d = track %d tc %d, want %d/%d", i, b.track, b.tcMs, want[i].track, want[i].tc)
		}
	}
	if !blocks[0].keyframe {
		t.Error("first video block must be flagged keyframe")
	}
}

func TestMuxerWindowRollover(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(640, 360)
	m.SetAudio(44100, 2, nil)
	_ = m.WriteHeader()

	for i := 0; i < 20; i++ { // 20 frames @60fps ≈ 333ms; feed 2s worth × 12 windows
		_ = m.Feed(TrackVideo, int64(i)*17, i == 0, []byte{byte(i)})
		_ = m.Feed(TrackAudio, int64(i)*16, false, []byte{byte(i)})
	}
	// One far-ahead frame flushes the first window.
	_ = m.Feed(TrackVideo, 6000, true, []byte{0xEE})
	_ = m.Flush()

	_, clusters := parseMuxed(t, buf.Bytes())
	if len(clusters) != 2 {
		t.Fatalf("clusters = %d, want 2 (window rollover)", len(clusters))
	}
	var total int
	for _, c := range clusters {
		total += len(c)
	}
	if total != 41 {
		t.Errorf("total blocks = %d, want 41", total)
	}
}

func TestMuxerOverlapDedup(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(640, 360)
	m.SetAudio(44100, 2, nil)
	_ = m.WriteHeader()

	_ = m.Feed(TrackVideo, 1000, true, []byte("x"))
	_ = m.Feed(TrackVideo, 1000, true, []byte("dup")) // same timecode: dropped
	_ = m.Feed(TrackVideo, 984, false, []byte("old")) // before cursor: dropped
	_ = m.Feed(TrackVideo, 1016, false, []byte("y"))
	_ = m.Flush()

	_, clusters := parseMuxed(t, buf.Bytes())
	var blocks []blockOut
	for _, c := range clusters {
		blocks = append(blocks, c...)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (dedup failed)", len(blocks))
	}
	if string(blocks[0].data) != "x" || string(blocks[1].data) != "y" {
		t.Errorf("blocks = %q %q", blocks[0].data, blocks[1].data)
	}
}

func TestMuxerBaseNormalization(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(640, 360)
	m.SetAudio(44100, 2, nil)
	_ = m.WriteHeader()
	// Broadcast-time base: output must normalize to ~0.
	_ = m.Feed(TrackVideo, 18751018133, true, []byte("f"))
	_ = m.Feed(TrackVideo, 18751018150, false, []byte("g"))
	_ = m.Flush()

	_, clusters := parseMuxed(t, buf.Bytes())
	if len(clusters) != 1 || len(clusters[0]) != 2 {
		t.Fatalf("clusters=%v", clusters)
	}
	if clusters[0][0].tcMs != 0 || clusters[0][1].tcMs != 17 {
		t.Errorf("timecodes = %d, %d; want 0, 17 (base normalization)", clusters[0][0].tcMs, clusters[0][1].tcMs)
	}
}

func TestMuxerFeedBeforeHeader(t *testing.T) {
	var buf bytes.Buffer
	m := New(&buf)
	m.SetVideo(640, 360)
	m.SetAudio(44100, 2, nil)
	// Frames offered before the header are buffered, not rejected: the
	// audio demuxer's frames arrive before the video metadata upstream.
	if err := m.Feed(TrackAudio, 10, false, []byte("early")); err != nil {
		t.Fatalf("Feed before WriteHeader must buffer, got %v", err)
	}
	if err := m.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	_, clusters := parseMuxed(t, buf.Bytes())
	if len(clusters) != 1 || len(clusters[0]) != 1 {
		t.Fatalf("clusters = %v, want one block released after the header", clusters)
	}
	// The buffered block is the first of the stream: its timecode
	// normalizes to zero.
	if clusters[0][0].tcMs != 0 {
		t.Errorf("buffered block tc = %d, want 0", clusters[0][0].tcMs)
	}
}
