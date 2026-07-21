package controllers

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
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
