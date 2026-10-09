package controllers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
)

// fakeExtractor is a test double for extractor.Extractor that counts
// Extract calls and records the format requested on each call.
type fakeExtractor struct {
	mu          sync.Mutex
	calls       int
	callFormats []string
	url         string
	headers     http.Header
	formats     []string
	err         error
}

func (f *fakeExtractor) Extract(format string) (*extractor.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.callFormats = append(f.callFormats, format)
	if f.err != nil {
		return nil, f.err
	}
	return &extractor.Result{URL: f.url, Headers: f.headers}, nil
}

func (f *fakeExtractor) SupportedFormats() []string { return f.formats }
func (f *fakeExtractor) DefaultFormat() string      { return f.formats[0] }

func (f *fakeExtractor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeExtractor) lastFormats() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.callFormats))
	copy(out, f.callFormats)
	return out
}

// TestBuildExtractFn_CachesFirstResult verifies that the first extraction
// (previous==nil) is cached: subsequent nil calls return the same result
// pointer without hitting the extractor again. This is the direct regression
// for the dispatch+forwarder double-extract bug.
func TestBuildExtractFn_CachesFirstResult(t *testing.T) {
	ext := &fakeExtractor{
		url:     "wss://host/live/test.xs",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "m3u8")

	r1, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	r2, err := fn(nil)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if ext.callCount() != 1 {
		t.Errorf("expected extractor to be called once, got %d", ext.callCount())
	}
	if r1 != r2 {
		t.Error("expected the same cached result pointer, got distinct results")
	}
	if r1.URL != "wss://host/live/test.xs" {
		t.Errorf("unexpected url %q", r1.URL)
	}
	if fmts := ext.lastFormats(); len(fmts) != 1 || fmts[0] != "m3u8" {
		t.Errorf("expected single Extract with format m3u8, got %v", fmts)
	}
}

// TestBuildExtractFn_RetryReextracts verifies that a retry (previous!=nil)
// performs a fresh extraction using the initial format, returning a new
// result when the format is consistent.
func TestBuildExtractFn_RetryReextracts(t *testing.T) {
	ext := &fakeExtractor{
		url:     "wss://host/live/test.xs",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "m3u8")

	first, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	second, err := fn(first)
	if err != nil {
		t.Fatalf("retry extract: %v", err)
	}
	if ext.callCount() != 2 {
		t.Errorf("expected 2 Extract calls, got %d", ext.callCount())
	}
	// Retry re-extracts using the resolved initial format ("ws"), not the
	// originally-desired "m3u8".
	if fmts := ext.lastFormats(); len(fmts) != 2 || fmts[1] != "ws" {
		t.Errorf("expected retry format ws, got %v", fmts)
	}
	if second.URL != "wss://host/live/test.xs" {
		t.Errorf("unexpected url %q", second.URL)
	}
}

// TestBuildExtractFn_FormatMismatchOnRetry verifies that a retry returning a
// different format than the initial extraction yields an error so the caller
// can retry instead of switching formats mid-stream.
func TestBuildExtractFn_FormatMismatchOnRetry(t *testing.T) {
	ext := &fakeExtractor{
		url:     "wss://host/live/test.xs",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "m3u8")

	first, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	// Flip the upstream URL to a different format on retry.
	ext.url = "http://host/live/test.flv"
	if _, err := fn(first); err == nil {
		t.Fatal("expected format mismatch error on retry, got nil")
	}
}

// TestBuildExtractFn_ExtractError verifies that a failed first extraction is
// cached: subsequent nil calls return the same error without re-invoking the
// extractor (matching the original controller behavior of failing fast).
func TestBuildExtractFn_ExtractError(t *testing.T) {
	ext := &fakeExtractor{
		err:     errors.New("boom"),
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "m3u8")

	if _, err := fn(nil); err == nil {
		t.Fatal("expected error on first extract, got nil")
	}
	if _, err := fn(nil); err == nil {
		t.Fatal("expected cached error on second nil extract, got nil")
	}
	if ext.callCount() != 1 {
		t.Errorf("expected extractor called once (error cached), got %d", ext.callCount())
	}
}

// TestBuildExtractFn_FormatMismatchRetriesThenDeadlock verifies that a
// persistent format change on retry (e.g. DouYu switching a room to p2p
// mid-stream) is tolerated for a few attempts and then reported as a
// terminal ErrFormatDeadlock so the forwarder can stop looping and let the
// client reconnect into the right forwarder.
func TestBuildExtractFn_FormatMismatchRetriesThenDeadlock(t *testing.T) {
	ext := &fakeExtractor{
		url:     "https://host/live/test.flv",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "flv")

	first, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}

	// The platform now serves the room as p2p: every retry yields ws.
	ext.url = "wss://host/live/test.xs"
	for i := 1; i <= formatDeadlockThreshold; i++ {
		_, err := fn(first)
		if err == nil {
			t.Fatalf("attempt %d: expected mismatch error, got nil", i)
		}
		if errors.Is(err, stream.ErrFormatDeadlock) {
			t.Fatalf("attempt %d: deadlock fired too early (threshold %d)", i, formatDeadlockThreshold)
		}
	}

	// Next attempt must be terminal.
	_, err = fn(first)
	if err == nil {
		t.Fatal("expected terminal deadlock error, got nil")
	}
	if !errors.Is(err, stream.ErrFormatDeadlock) {
		t.Fatalf("expected ErrFormatDeadlock after %d mismatches, got: %v", formatDeadlockThreshold+1, err)
	}
}

// TestBuildExtractFn_FormatMismatchCounterResets verifies the mismatch
// counter resets after a format-consistent extraction, so a one-off blip
// never accumulates toward a deadlock.
func TestBuildExtractFn_FormatMismatchCounterResets(t *testing.T) {
	ext := &fakeExtractor{
		url:     "https://host/live/test.flv",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "flv")

	first, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}

	// One mismatch, then back to the original format.
	ext.url = "wss://host/live/test.xs"
	if _, err := fn(first); err == nil {
		t.Fatal("expected mismatch error on ws retry")
	}
	ext.url = "https://host/live/test.flv"
	if _, err := fn(first); err != nil {
		t.Fatalf("expected consistent retry to succeed: %v", err)
	}

	// Threshold mismatches in a row must still deadlock (counter was reset).
	ext.url = "wss://host/live/test.xs"
	for i := 0; i < formatDeadlockThreshold; i++ {
		if _, err := fn(first); err == nil {
			t.Fatal("expected mismatch errors")
		}
	}
	_, err = fn(first)
	if !errors.Is(err, stream.ErrFormatDeadlock) {
		t.Fatalf("expected deadlock after %d consecutive mismatches, got: %v", formatDeadlockThreshold+1, err)
	}
}

// TestBuildExtractFn_XSIsFlvFamily verifies that a retry returning a DouYu
// p2p=2 .xs URL is treated as the same format as the original .flv URL:
// dispatchStream already routes .xs to the FLV forwarder, so the stream
// continues seamlessly instead of reporting a format mismatch.
func TestBuildExtractFn_XSIsFlvFamily(t *testing.T) {
	ext := &fakeExtractor{
		url:     "https://host/live/test.flv",
		formats: []string{"flv", "m3u8", "ws"},
	}
	fn := buildExtractFn(ext, "flv")

	first, err := fn(nil)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if first.URL != "https://host/live/test.flv" {
		t.Fatalf("unexpected first url %q", first.URL)
	}

	// Server switches to p2p=2: same http-flv family, .xs extension.
	ext.url = "https://hlsh5p2.douyucdn2.cn/live/test.xs?txSecret=s"
	second, err := fn(first)
	if err != nil {
		t.Fatalf("xs retry must be accepted as flv family: %v", err)
	}
	if second.URL != ext.url {
		t.Fatalf("unexpected retry url %q", second.URL)
	}
	if fmts := ext.lastFormats(); len(fmts) != 2 || fmts[1] != "flv" {
		t.Errorf("expected retry with initial format flv, got %v", fmts)
	}
}

// fakeQualityExtractor extends fakeExtractor with a QualitySetter recording
// every quality hint it receives.
type fakeQualityExtractor struct {
	fakeExtractor
	mu        sync.Mutex
	qualities []string
}

func (f *fakeQualityExtractor) SetQuality(quality string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qualities = append(f.qualities, quality)
}

func (f *fakeQualityExtractor) receivedQualities() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.qualities))
	copy(out, f.qualities)
	return out
}

func newTestContext(t *testing.T, target string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return c, w
}

// TestApplyQualityHint verifies that the ?quality= query parameter is
// injected into extractors implementing QualitySetter, and that extractors
// without the interface (or requests without the parameter) are untouched.
func TestApplyQualityHint(t *testing.T) {
	t.Run("injects quality when supported", func(t *testing.T) {
		ext := &fakeQualityExtractor{fakeExtractor: fakeExtractor{
			url:     "https://host/live/index.m3u8",
			formats: []string{"m3u8"},
		}}
		c, _ := newTestContext(t, "/youtube/WWTcu33u00A?quality=720p")
		applyQualityHint(c, ext)
		if q := ext.receivedQualities(); len(q) != 1 || q[0] != "720p" {
			t.Errorf("expected quality 720p to be injected, got %v", q)
		}
	})

	t.Run("missing quality param is a no-op", func(t *testing.T) {
		ext := &fakeQualityExtractor{fakeExtractor: fakeExtractor{
			url:     "https://host/live/index.m3u8",
			formats: []string{"m3u8"},
		}}
		c, _ := newTestContext(t, "/youtube/WWTcu33u00A")
		applyQualityHint(c, ext)
		if q := ext.receivedQualities(); len(q) != 0 {
			t.Errorf("expected no quality injection, got %v", q)
		}
	})

	t.Run("empty quality param is a no-op", func(t *testing.T) {
		ext := &fakeQualityExtractor{fakeExtractor: fakeExtractor{
			url:     "https://host/live/index.m3u8",
			formats: []string{"m3u8"},
		}}
		c, _ := newTestContext(t, "/youtube/WWTcu33u00A?quality=")
		applyQualityHint(c, ext)
		if q := ext.receivedQualities(); len(q) != 0 {
			t.Errorf("expected no quality injection, got %v", q)
		}
	})

	t.Run("extractor without QualitySetter is ignored", func(t *testing.T) {
		ext := &fakeExtractor{
			url:     "https://host/live/index.m3u8",
			formats: []string{"m3u8"},
		}
		c, _ := newTestContext(t, "/kick/somechannel?quality=720p")
		applyQualityHint(c, ext) // must not panic
	})
}
