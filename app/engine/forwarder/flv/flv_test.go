package flv

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestHeaderCache_GetOrCreate(t *testing.T) {
	cache := NewHeaderCache()

	// Same key returns same entry.
	e1 := cache.GetOrCreate("douyu:12345")
	e2 := cache.GetOrCreate("douyu:12345")
	if e1 != e2 {
		t.Fatal("GetOrCreate with same key should return same entry")
	}

	// Different keys return different entries.
	e3 := cache.GetOrCreate("huya:67890")
	if e1 == e3 {
		t.Fatal("GetOrCreate with different keys should return different entries")
	}

	// Re-get the first key still returns the same entry.
	e4 := cache.GetOrCreate("douyu:12345")
	if e1 != e4 {
		t.Fatal("GetOrCreate should consistently return same entry for same key")
	}
}

func TestHeaderEntry_SetWait(t *testing.T) {
	e := newHeaderEntry()

	// Wait should block until Set is called, so call Set in a goroutine.
	data := []byte{0x01, 0x02, 0x03}
	go e.Set(data)

	e.Wait()

	got := e.Data()
	if !bytes.Equal(got, data) {
		t.Fatalf("Data() = %v, want %v", got, data)
	}

	// Set again with new data — Wait should not block since ready is already closed.
	newData := []byte{0x04, 0x05, 0x06, 0x07}
	e.Set(newData)

	got = e.Data()
	if !bytes.Equal(got, newData) {
		t.Fatalf("Data() after second Set = %v, want %v", got, newData)
	}

	// Original data slice should not be affected by later mutations.
	orig := []byte{0xAA}
	e2 := newHeaderEntry()
	go e2.Set(orig)
	e2.Wait()
	orig[0] = 0xBB
	if e2.Data()[0] != 0xAA {
		t.Fatal("Set should copy data, not hold a reference to the original slice")
	}
}

func TestIsFLVConfigTag(t *testing.T) {
	tests := []struct {
		name    string
		tagType byte
		data    []byte
		want    bool
	}{
		{
			name:    "Script Data (0x12)",
			tagType: 0x12,
			data:    []byte{0x02, 0x00},
			want:    true,
		},
		{
			name:    "Audio AAC Sequence Header",
			tagType: 0x08,
			data:    []byte{0xAF, 0x00}, // soundFormat=10 (AAC), AAC Sequence Header
			want:    true,
		},
		{
			name:    "Audio AAC not Sequence Header",
			tagType: 0x08,
			data:    []byte{0xAF, 0x01}, // soundFormat=10, AAC raw
			want:    false,
		},
		{
			name:    "Audio non-AAC",
			tagType: 0x08,
			data:    []byte{0x0F, 0x00}, // soundFormat != 10
			want:    false,
		},
		{
			name:    "Audio too short",
			tagType: 0x08,
			data:    []byte{0xAF},
			want:    false,
		},
		{
			name:    "Video AVC keyframe Sequence Header",
			tagType: 0x09,
			data:    []byte{0x17, 0x00}, // keyframe + AVC, AVC Sequence Header
			want:    true,
		},
		{
			name:    "Video HEVC keyframe Sequence Header",
			tagType: 0x09,
			data:    []byte{0x1C, 0x00}, // keyframe + HEVC(codecID=12), HEVC Sequence Header
			want:    true,
		},
		{
			name:    "Video non-keyframe",
			tagType: 0x09,
			data:    []byte{0x27, 0x00}, // inter frame + AVC
			want:    false,
		},
		{
			name:    "Video keyframe AVC not Sequence Header",
			tagType: 0x09,
			data:    []byte{0x17, 0x01}, // keyframe + AVC, AVC NALU
			want:    false,
		},
		{
			name:    "Video too short",
			tagType: 0x09,
			data:    []byte{0x17},
			want:    false,
		},
		{
			name:    "Unknown tag type",
			tagType: 0x05,
			data:    []byte{0x00},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFLVConfigTag(tt.tagType, tt.data)
			if got != tt.want {
				t.Errorf("isFLVConfigTag(0x%02X, %v) = %v, want %v", tt.tagType, tt.data, got, tt.want)
			}
		})
	}
}

func TestHeaderCacheWriter_DetectHeaderBoundary(t *testing.T) {
	cache := NewHeaderCache()

	t.Run("valid FLV header with config tags", func(t *testing.T) {
		// Build a minimal FLV stream:
		// FLV header (9 bytes) + PreviousTagSize0 (4 bytes)
		// + Script Data tag (config)
		// + Audio AAC Sequence Header tag (config)
		// + Video AVC keyframe NALU tag (media — boundary)
		flvHeader := []byte{'F', 'L', 'V', 0x01, 0x05, 0x00, 0x00, 0x00, 0x09}
		prevTagSize0 := []byte{0x00, 0x00, 0x00, 0x00}

		// Script Data tag: type=0x12, dataSize=2, timestamp=0, streamID=0, data, prevTagSize
		scriptData := []byte{0x02, 0x00} // minimal payload
		scriptTag := buildFLVTag(0x12, scriptData)

		// Audio AAC Sequence Header tag: type=0x08, data=[0xAF, 0x00]
		audioSeqHeader := []byte{0xAF, 0x00}
		audioTag := buildFLVTag(0x08, audioSeqHeader)

		// Video AVC keyframe NALU (NOT config): type=0x09, data=[0x17, 0x01]
		videoNALU := []byte{0x17, 0x01}
		videoTag := buildFLVTag(0x09, videoNALU)

		fullData := append(append(append(append(
			[]byte{}, flvHeader...), prevTagSize0...), scriptTag...), audioTag...)
		expectedBoundary := len(fullData)
		fullData = append(fullData, videoTag...)

		var out bytes.Buffer
		w := NewHeaderCacheWriter(&out, cache, "test:valid")
		n, err := w.Write(fullData)
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if n != len(fullData) {
			t.Fatalf("Write returned %d, want %d", n, len(fullData))
		}

		// The header should be cached with the correct size.
		entry := cache.GetOrCreate("test:valid")
		entry.Wait()
		data := entry.Data()
		if len(data) != expectedBoundary {
			t.Fatalf("cached header length = %d, want %d", len(data), expectedBoundary)
		}

		// The header must be stripped from the output: only the media tag
		// reaches the pipe. Consumers prepend the cached header themselves,
		// so leaving it in would duplicate it for every warm-cache client.
		if !bytes.Equal(out.Bytes(), videoTag) {
			t.Fatalf("pipe output = %v, want media tag only = %v", out.Bytes(), videoTag)
		}
	})

	t.Run("invalid data", func(t *testing.T) {
		cache2 := NewHeaderCache()
		w := NewHeaderCacheWriter(io.Discard, cache2, "test:invalid")
		// Write data that doesn't start with "FLV" — should passthrough.
		invalidData := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D}
		n, err := w.Write(invalidData)
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if n != len(invalidData) {
			t.Fatalf("Write returned %d, want %d", n, len(invalidData))
		}

		// The cache entry must be resolved as missing so consumers waiting
		// for a header stop waiting instead of hanging forever.
		entry := cache2.GetOrCreate("test:invalid")
		if !entry.IsReady() {
			t.Fatal("non-FLV passthrough should resolve the cache entry as missing")
		}
		if entry.Data() != nil {
			t.Fatalf("Data() = %v, want nil after non-FLV passthrough", entry.Data())
		}
	})

	t.Run("incomplete data", func(t *testing.T) {
		cache3 := NewHeaderCache()
		w := NewHeaderCacheWriter(io.Discard, cache3, "test:incomplete")
		// Write less than 13 bytes — should buffer and not crash.
		partial := []byte{'F', 'L', 'V', 0x01, 0x05, 0x00, 0x00, 0x00, 0x09, 0x00, 0x00, 0x00}
		n, err := w.Write(partial)
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if n != len(partial) {
			t.Fatalf("Write returned %d, want %d", n, len(partial))
		}

		// Entry should not have data set yet (still detecting).
		entry := cache3.GetOrCreate("test:incomplete")
		// Don't call Wait() here — it would block forever since Set hasn't been called.
		// Just verify Data() returns nil.
		if entry.Data() != nil {
			t.Fatal("Data() should be nil for incomplete detection")
		}
	})
}

// buildFLVTag constructs a minimal FLV tag (tag header + data + previous tag size).
func buildFLVTag(tagType byte, data []byte) []byte {
	dataSize := len(data)
	tag := make([]byte, 11+dataSize+4)
	tag[0] = tagType
	tag[1] = byte(dataSize >> 16)
	tag[2] = byte(dataSize >> 8)
	tag[3] = byte(dataSize)
	// timestamp bytes [4:7] = 0
	// timestamp extended [8] = 0
	// streamID [9:11] = 0
	copy(tag[11:], data)
	// PreviousTagSize at the end.
	prevSize := 11 + dataSize
	tag[11+dataSize] = byte(prevSize >> 24)
	tag[11+dataSize+1] = byte(prevSize >> 16)
	tag[11+dataSize+2] = byte(prevSize >> 8)
	tag[11+dataSize+3] = byte(prevSize)
	return tag
}

func TestFLVStream_ReadWithHeader(t *testing.T) {
	cache := NewHeaderCache()
	key := "test:flvstream"
	headerData := []byte{0x46, 0x4C, 0x56, 0x01, 0x05} // "FLV\x01\x05"

	// Pre-populate the cache with header data.
	entry := cache.GetOrCreate(key)
	entry.Set(headerData)

	// Build the inner stream using NewStream with a fetch function that
	// returns data from a controlled pipe reader.
	liveData := []byte("live-stream-data")

	pr, pw := io.Pipe()
	callCount := 0
	extractFn := func(previous *stream.ExtractResult) (*stream.ExtractResult, error) {
		callCount++
		return &stream.ExtractResult{URL: "http://example.com/live.flv"}, nil
	}

	fetchFn := func(u string, headers http.Header) (io.ReadCloser, error) {
		return pr, nil
	}

	innerStream := stream.NewStream(extractFn, fetchFn)

	// Write live data then close the pipe writer to signal EOF.
	go func() {
		pw.Write(liveData)
		pw.Close()
	}()

	flvStream := NewFLVStream(innerStream, cache, key)

	// Read should get header first, then live data.
	buf := make([]byte, 1024)

	// First read: header data.
	n, err := flvStream.Read(buf)
	if err != nil {
		t.Fatalf("first Read returned error: %v", err)
	}
	if !bytes.Equal(buf[:n], headerData) {
		t.Fatalf("first Read got %v, want %v", buf[:n], headerData)
	}

	// Second read: live data.
	n, err = flvStream.Read(buf)
	if err != nil {
		t.Fatalf("second Read returned error: %v", err)
	}
	if !bytes.Equal(buf[:n], liveData) {
		t.Fatalf("second Read got %v, want %v", buf[:n], liveData)
	}

	// The inner stream's produce goroutine is still running after the pipe
	// writer closes. Since closeWithError does not use closeOnce, calling
	// Close() here would panic with "close of closed channel". We skip
	// cleanup — the goroutine will exit on its own after the pipe error
	// propagates. This test only validates the Read (header + live) behavior.
}

func TestHeaderEntry_SetMissingWaitTimeout(t *testing.T) {
	e := newHeaderEntry()

	if e.IsReady() {
		t.Fatal("fresh entry should not be ready")
	}
	if e.WaitTimeout(10 * time.Millisecond) {
		t.Fatal("WaitTimeout should time out on an unresolved entry")
	}

	e.SetMissing()
	if !e.IsReady() {
		t.Fatal("SetMissing should resolve the entry")
	}
	if e.Data() != nil {
		t.Fatalf("Data() = %v, want nil after SetMissing", e.Data())
	}
	if !e.WaitTimeout(time.Second) {
		t.Fatal("WaitTimeout should return immediately on a resolved entry")
	}

	// A later Set recovers the entry with data (e.g. a reconnect delivers a
	// valid FLV stream after a garbage response).
	e.Set([]byte{0x01, 0x02, 0x03})
	if !bytes.Equal(e.Data(), []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("Data() = %v, want the data from the recovering Set", e.Data())
	}
}

// buildFLVStream builds a complete synthetic FLV stream: file header,
// config tags (script data, AAC sequence header, AVC sequence header), then
// mediaCount raw AAC audio tags. It returns the full stream and the header
// (everything up to and including the last config tag) — the part
// HeaderCacheWriter is expected to cache and strip.
func buildFLVStream(mediaCount int) (full, header []byte) {
	flvHeader := []byte{'F', 'L', 'V', 0x01, 0x05, 0x00, 0x00, 0x00, 0x09}
	prevTagSize0 := []byte{0x00, 0x00, 0x00, 0x00}
	scriptTag := buildFLVTag(0x12, []byte{0x02, 0x00})
	audioSeqTag := buildFLVTag(0x08, []byte{0xAF, 0x00}) // AAC sequence header (config)
	videoSeqTag := buildFLVTag(0x09, []byte{0x17, 0x00}) // AVC keyframe sequence header (config)

	header = append(append(append(append(append([]byte{}, flvHeader...), prevTagSize0...), scriptTag...), audioSeqTag...), videoSeqTag...)
	full = append([]byte(nil), header...)
	for i := 0; i < mediaCount; i++ {
		full = append(full, buildFLVTag(0x08, []byte{0xAF, 0x01})...) // raw AAC frame (media)
	}
	return full, header
}

// stopFetchErr terminates the produce loop with a non-retriable error so
// tests can read the stream to completion.
var stopFetchErr = errors.New("stop test stream")

// stopAfter returns a FetchFunc that serves the given payloads in order,
// one per produce iteration, then fails with stopFetchErr.
func stopAfter(payloads ...[]byte) stream.FetchFunc {
	calls := 0
	return func(u string, headers http.Header) (io.ReadCloser, error) {
		if calls >= len(payloads) {
			return nil, stopFetchErr
		}
		payload := payloads[calls]
		calls++
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
}

// newTestFLVStream wires a full per-request FLV pipeline the same way
// flvStreamWithCache does in the controller: a producer stream whose pipe is
// wrapped by a HeaderCacheWriter, plus the FLVStream that prepends the
// cached header.
func newTestFLVStream(cache *HeaderCache, key string, fetchFn stream.FetchFunc) *FLVStream {
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: "http://example.com/live.flv"}, nil
	}
	writerWrapper := func(w io.Writer) io.Writer {
		return NewHeaderCacheWriter(w, cache, key)
	}
	inner := stream.NewStream(extractFn, fetchFn, stream.WithWriterWrapper(writerWrapper))
	return NewFLVStream(inner, cache, key)
}

// TestFLVStream_ColdClientSingleHeader verifies the cold-cache path: the
// client's own producer caches the header, and the client receives exactly
// one header followed by the media data.
func TestFLVStream_ColdClientSingleHeader(t *testing.T) {
	cache := NewHeaderCache()
	key := "test:cold"
	full, _ := buildFLVStream(4)

	f := newTestFLVStream(cache, key, stopAfter(full))
	data, err := io.ReadAll(f)
	if !errors.Is(err, stopFetchErr) {
		t.Fatalf("ReadAll error = %v, want stopFetchErr", err)
	}
	if !bytes.Equal(data, full) {
		t.Fatalf("client stream = %d bytes, want the single-header stream (%d bytes)", len(data), len(full))
	}
	if got := bytes.Count(data, []byte("FLV")); got != 1 {
		t.Fatalf("client stream contains %d FLV signatures, want 1", got)
	}
}

// TestFLVStream_WarmClientSingleHeader is the regression test for the
// doubled-header bug: a client connecting while the header cache is warm
// must receive the cached header exactly once, followed by the media data of
// its own fresh upstream connection — whose own header must be stripped.
// Before the fix, the new connection's header passed through the pipe
// unstripped, so every client after the first received two overlapping FLV
// headers, which desynced demuxers (PotPlayer refused to play, ffprobe
// reported "Packet mismatch" with garbage tag sizes derived from the second
// 'FLV' signature).
func TestFLVStream_WarmClientSingleHeader(t *testing.T) {
	cache := NewHeaderCache()
	key := "test:warm"
	_, header := buildFLVStream(0)
	full2, _ := buildFLVStream(3)

	// A previous client warmed the cache.
	cache.GetOrCreate(key).Set(header)

	f := newTestFLVStream(cache, key, stopAfter(full2))
	data, err := io.ReadAll(f)
	if !errors.Is(err, stopFetchErr) {
		t.Fatalf("ReadAll error = %v, want stopFetchErr", err)
	}
	want := append(append([]byte{}, header...), full2[len(header):]...)
	if !bytes.Equal(data, want) {
		t.Fatalf("client stream = %d bytes, want cached header + new media (%d bytes)", len(data), len(want))
	}
	if got := bytes.Count(data, []byte("FLV")); got != 1 {
		t.Fatalf("client stream contains %d FLV signatures, want 1", got)
	}
}

// TestFLVStream_ReconnectStripsHeader is the regression test for the
// mid-stream header splice: when the upstream connection is replaced
// (403/expiry reconnect), the new connection delivers a complete FLV stream
// again — header included. The header must be stripped so the client sees
// continuous media data with no second FLV signature in the middle.
func TestFLVStream_ReconnectStripsHeader(t *testing.T) {
	cache := NewHeaderCache()
	key := "test:reconnect"
	full1, header := buildFLVStream(2)
	full2, _ := buildFLVStream(3)

	f := newTestFLVStream(cache, key, stopAfter(full1, full2))
	data, err := io.ReadAll(f)
	if !errors.Is(err, stopFetchErr) {
		t.Fatalf("ReadAll error = %v, want stopFetchErr", err)
	}
	want := append(append([]byte{}, full1...), full2[len(header):]...)
	if !bytes.Equal(data, want) {
		t.Fatalf("client stream = %d bytes, want both connections' media with a single header (%d bytes)", len(data), len(want))
	}
	if got := bytes.Count(data, []byte("FLV")); got != 1 {
		t.Fatalf("client stream contains %d FLV signatures, want 1", got)
	}
}

// TestFLVStream_HeaderWaitTimeout verifies the safety net: when the header
// never becomes available (e.g. the upstream stalls mid-detection), Read
// gives up after HeaderWaitTimeout and passes the inner stream through
// instead of hanging forever.
func TestFLVStream_HeaderWaitTimeout(t *testing.T) {
	old := HeaderWaitTimeout
	HeaderWaitTimeout = 100 * time.Millisecond
	defer func() { HeaderWaitTimeout = old }()

	cache := NewHeaderCache()
	key := "test:wait-timeout"
	media := buildFLVTag(0x08, []byte{0xAF, 0x01})

	// No writer wrapper: nothing ever populates the cache entry.
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: "http://example.com/live.flv"}, nil
	}
	inner := stream.NewStream(extractFn, stopAfter(media))
	f := NewFLVStream(inner, cache, key)

	start := time.Now()
	data, err := io.ReadAll(f)
	elapsed := time.Since(start)
	if !errors.Is(err, stopFetchErr) {
		t.Fatalf("ReadAll error = %v, want stopFetchErr", err)
	}
	if elapsed < HeaderWaitTimeout {
		t.Fatalf("ReadAll returned after %v, want at least the header wait timeout %v", elapsed, HeaderWaitTimeout)
	}
	if !bytes.Equal(data, media) {
		t.Fatalf("client stream = %v, want the media data passed through", data)
	}
}

// TestFLVStream_MissingHeaderPassthrough guards the first-play black screen
// that c109f4f fixed: when the upstream is not valid FLV, the writer
// resolves the cache entry as missing and the client must receive the raw
// data immediately, without waiting for a header that will never come.
func TestFLVStream_MissingHeaderPassthrough(t *testing.T) {
	cache := NewHeaderCache()
	key := "test:missing"
	raw := []byte("not an flv stream, just raw upstream bytes")

	f := newTestFLVStream(cache, key, stopAfter(raw))

	start := time.Now()
	data, err := io.ReadAll(f)
	if !errors.Is(err, stopFetchErr) {
		t.Fatalf("ReadAll error = %v, want stopFetchErr", err)
	}
	if !bytes.Equal(data, raw) {
		t.Fatalf("client stream = %q, want raw passthrough %q", data, raw)
	}
	if elapsed := time.Since(start); elapsed >= HeaderWaitTimeout {
		t.Fatalf("passthrough took %v, should not wait for the header timeout", elapsed)
	}
}
