package dash

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	// Speed up polling, session renewal and failure limits so tests run fast.
	pollInterval = 50 * time.Millisecond
	sessionRenewInterval = 300 * time.Millisecond
	notFoundLimit = 3
	renewBackoffBase = 150 * time.Millisecond
	renewBackoffMax = 400 * time.Millisecond
	os.Exit(m.Run())
}

// readAllUntilClose reads everything the stream produces until it is closed.
func readAllUntilClose(t *testing.T, s *DASHStream, d time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(s)
		ch <- b
	}()
	time.Sleep(d)
	s.Close()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close within 5s")
		return nil
	}
}

// trackServer serves MPD + per-track batches. sq/100 returns tfdts
// [1000,2000], sq/101 returns [2000,3000] (overlapping 2000 on purpose),
// sq/102 returns [3000,4000], and anything else 404s. After expireAfter
// requests to a track, responses switch to 403 to force re-extraction.
type trackServer struct {
	mu          sync.Mutex
	audioSQ     map[int][]uint64 // sq -> tfdts
	videoSQ     map[int][]uint64
	audioReqs   int
	videoReqs   int
	extractSeen int32
}

func newTrackServer() *trackServer {
	return &trackServer{
		audioSQ: map[int][]uint64{
			100: {1000, 2000},
			101: {2000, 3000},
			102: {3000, 4000},
		},
		videoSQ: map[int][]uint64{
			100: {10000},
			101: {20000},
			102: {30000},
		},
	}
}

func (ts *trackServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			ts.mu.Lock()
			ts.audioReqs++
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			tfdts, ok := ts.audioSQ[sq]
			ts.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			ts.mu.Lock()
			ts.videoReqs++
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/video299/sq/"), "/", 2)[0])
			tfdts, ok := ts.videoSQ[sq]
			ts.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

var tsServerBase string

func TestDASHStream_Produce(t *testing.T) {
	tss := newTrackServer()
	ts := httptest.NewServer(tss.handler())
	defer ts.Close()
	tsServerBase = ts.URL

	var extractCount atomic.Int32
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		extractCount.Add(1)
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}

	s := NewDASHStream(extractFn, ts.Client())
	data := readAllUntilClose(t, s, 1500*time.Millisecond)

	if extractCount.Load() < 1 {
		t.Fatalf("extractFn never called")
	}

	// Walk the produced stream and verify structure.
	boxes := walkBoxes(data, 0, len(data))
	if len(boxes) < 8 {
		t.Fatalf("too few boxes produced (%d): % x", len(boxes), data)
	}

	// First three boxes: ftyp, moov, emsg (merged init).
	if boxes[0].typ != "ftyp" {
		t.Errorf("box[0] = %s, want ftyp", boxes[0].typ)
	}
	if boxes[1].typ != "moov" {
		t.Errorf("box[1] = %s, want moov", boxes[1].typ)
	}
	ftypCount, moovCount := 0, 0
	for _, b := range boxes {
		if b.typ == "ftyp" {
			ftypCount++
		}
		if b.typ == "moov" {
			moovCount++
		}
	}
	if ftypCount != 1 {
		t.Errorf("ftyp count = %d, want exactly 1 (repeated inits must be stripped)", ftypCount)
	}
	if moovCount != 1 {
		t.Errorf("moov count = %d, want exactly 1 (merged)", moovCount)
	}

	// Merged moov holds two traks with ids 1 and 2.
	moov := boxes[1]
	var trakIDs []uint32
	for _, b := range walkBoxes(data, moov.off+8, moov.off+moov.size) {
		if b.typ != "trak" {
			continue
		}
		tkhd := findBox(data, b.off+8, b.off+b.size, "tkhd")
		if tkhd == nil {
			t.Fatalf("trak without tkhd")
		}
		trakIDs = append(trakIDs, binary.BigEndian.Uint32(data[tkhd.off+8+12:tkhd.off+8+16]))
	}
	if len(trakIDs) != 2 || trakIDs[0] != 1 || trakIDs[1] != 2 {
		t.Errorf("merged moov trak ids = %v, want [1 2]", trakIDs)
	}

	// All media moofs after init: audio keep track_id 1, video rewritten to 2.
	// Deduplicate overlapping tfdt values per track; both tracks strictly
	// increasing.
	type moofInfo struct {
		trackID uint32
		tfdt    uint64
	}
	var moofs []moofInfo
	for _, b := range boxes {
		if b.typ != "moof" {
			continue
		}
		tfhd := findBox(data, b.off+8, b.off+b.size, "tfhd")
		// tfhd lives inside traf; search one level deeper.
		for _, traf := range walkBoxes(data, b.off+8, b.off+b.size) {
			if traf.typ != "traf" {
				continue
			}
			for _, tb := range walkBoxes(data, traf.off+8, traf.off+traf.size) {
				if tb.typ == "tfhd" {
					tfhd = &tb
				}
			}
		}
		if tfhd == nil {
			t.Fatalf("moof without tfhd at %d", b.off)
		}
		trackID := binary.BigEndian.Uint32(data[tfhd.off+8+4 : tfhd.off+8+8])
		tfdt, _ := moofTfdt(data, b.off, b.size)
		moofs = append(moofs, moofInfo{trackID, tfdt})
	}

	lastByTrack := map[uint32]uint64{}
	seen := map[uint32]map[uint64]bool{}
	audioMoofs, videoMoofs := 0, 0
	for i, m := range moofs {
		if m.trackID != 1 && m.trackID != 2 {
			t.Errorf("moofs[%d] track_id = %d, want 1 or 2", i, m.trackID)
		}
		if seen[m.trackID] == nil {
			seen[m.trackID] = map[uint64]bool{}
		}
		if seen[m.trackID][m.tfdt] {
			t.Errorf("moofs[%d] track %d tfdt %d duplicated (dedupe failed)", i, m.trackID, m.tfdt)
		}
		seen[m.trackID][m.tfdt] = true
		if prev, ok := lastByTrack[m.trackID]; ok && m.tfdt <= prev {
			t.Errorf("moofs[%d] track %d tfdt %d not increasing (prev %d)", i, m.trackID, m.tfdt, prev)
		}
		lastByTrack[m.trackID] = m.tfdt
		if m.trackID == 1 {
			audioMoofs++
		} else {
			videoMoofs++
		}
	}
	// At minimum the first round of batches must have been piped.
	if audioMoofs < 2 {
		t.Errorf("audio moofs piped = %d, want >= 2", audioMoofs)
	}
	if videoMoofs < 1 {
		t.Errorf("video moofs piped = %d, want >= 1", videoMoofs)
	}
}

func TestDASHStream_ReextractOn403(t *testing.T) {
	tss := newTrackServer()
	var failAudio atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			if failAudio.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			tss.mu.Lock()
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			tfdts, ok := tss.audioSQ[sq]
			tss.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			tss.mu.Lock()
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/video299/sq/"), "/", 2)[0])
			tfdts, ok := tss.videoSQ[sq]
			tss.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	var extractCount atomic.Int32
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		extractCount.Add(1)
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}

	s := NewDASHStream(extractFn, ts.Client())
	// Let one round complete, then break the audio track.
	time.Sleep(300 * time.Millisecond)
	failAudio.Store(true)
	data := readAllUntilClose(t, s, 2500*time.Millisecond)

	// The 403 must have triggered re-extraction (extractFn called again).
	if extractCount.Load() < 2 {
		t.Fatalf("extractFn called %d times, want >= 2 after 403", extractCount.Load())
	}

	// After recovery every piped init is a complete merged init (ftyp and
	// moov always paired) and no tfdt is ever piped twice across the
	// re-extraction (the tfdt cursor survives the reset).
	boxes := walkBoxes(data, 0, len(data))
	ftypCount, moovCount := 0, 0
	for _, b := range boxes {
		if b.typ == "ftyp" {
			ftypCount++
		}
		if b.typ == "moov" {
			moovCount++
		}
	}
	if ftypCount < 1 {
		t.Fatal("no init piped at all")
	}
	if moovCount != ftypCount {
		t.Errorf("after recovery ftyp=%d moov=%d, want them paired (merged inits only)", ftypCount, moovCount)
	}
	seen := map[uint64]bool{}
	for _, b := range boxes {
		if b.typ != "moof" {
			continue
		}
		tfdt, ok := moofTfdt(data, b.off, b.size)
		if !ok {
			continue
		}
		if seen[tfdt] {
			t.Errorf("tfdt %d piped twice across re-extraction (cursor lost)", tfdt)
		}
		seen[tfdt] = true
	}
}

func TestDASHStream_ExtractErrorRetries(t *testing.T) {
	calls := 0
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		calls++
		return nil, fmt.Errorf("extract boom")
	}
	s := NewDASHStream(extractFn, http.DefaultClient)
	data := readAllUntilClose(t, s, 3*time.Second)
	if len(data) != 0 {
		t.Errorf("expected no data on persistent extract error, got %d bytes", len(data))
	}
	if calls < 2 {
		t.Errorf("extract retried %d times, want >= 2", calls)
	}
}

// TestDASHStream_SegmentRequestURLNoLmt verifies the segment request URL
// carries no lmt suffix. The MPD's lmt values are per-segment version
// markers; a hardcoded /lmt/1 is rejected with 404 by part of the CDN fleet
// (observed on live rooms where the audio itag happened to tolerate it but
// every video itag 404'd), so the request must be issued without one.
func TestDASHStream_SegmentRequestURLNoLmt(t *testing.T) {
	var gotPath atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := &DASHStream{hc: ts.Client()}
	tr := &trackState{name: "video", baseURL: ts.URL + "/video299/", sq: 100}
	resp, err := s.doTrackRequest(tr, nil)
	if err != nil {
		t.Fatalf("doTrackRequest error: %v", err)
	}
	resp.Body.Close()
	if p, _ := gotPath.Load().(string); p != "/video299/sq/100" {
		t.Errorf("request path = %q, want /video299/sq/100 (no lmt suffix)", p)
	}
}

// TestDASHStream_InitProbe404Reextracts verifies that an init probe failing
// with a non-expired error (404) does not retry forever: after notFoundLimit
// consecutive failures the stream re-extracts, so a probe that can never
// succeed cycles through fresh extractions instead of leaving the client
// waiting on an empty response forever.
func TestDASHStream_InitProbe404Reextracts(t *testing.T) {
	tss := newTrackServer()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			tss.mu.Lock()
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			tfdts, ok := tss.audioSQ[sq]
			tss.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			// Video always 404s: the init probe can never succeed.
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	var extractCount atomic.Int32
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		extractCount.Add(1)
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}

	s := NewDASHStream(extractFn, ts.Client())
	_ = readAllUntilClose(t, s, 1500*time.Millisecond)

	// notFoundLimit = 3 in tests: the probe fails 3 times (~150ms), then the
	// stream must re-extract. At least one re-extraction within 1.5s proves
	// the loop has an exit.
	if extractCount.Load() < 2 {
		t.Fatalf("extractFn called %d times, want >= 2 (init probe 404 must re-extract after notFoundLimit failures)", extractCount.Load())
	}
}

// TestDASHStream_RenewFailureBackoff verifies that a failing renewal (the
// extractFn erroring during the periodic session renew) backs off
// exponentially instead of hammering the extractor API every pollInterval.
func TestDASHStream_RenewFailureBackoff(t *testing.T) {
	// The track server serves every sq (tfdt derived from sq, strictly
	// increasing), so batches never 404 and only renewals call the extractor.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			w.Write(mkBatch(1, []uint64{uint64(sq) * 1000}))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/video299/sq/"), "/", 2)[0])
			w.Write(mkBatch(1, []uint64{uint64(sq) * 20000}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	var calls atomic.Int32
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		if calls.Add(1) == 1 {
			return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
		}
		return nil, fmt.Errorf("renew extract boom")
	}

	s := NewDASHStream(extractFn, ts.Client())
	_ = readAllUntilClose(t, s, 1500*time.Millisecond)

	// sessionRenewInterval = 300ms in tests. Without backoff the loop would
	// retry every pollInterval (50ms) — ~25 calls in 1.5s. With the backoff
	// ladder (150ms→300ms→400ms cap) expect at most ~6.
	if n := calls.Load(); n > 6 {
		t.Errorf("extractFn called %d times in 1.5s, want <= 6 (renew failure must back off)", n)
	}
}

// TestDASHStream_InitProbesRunInParallel verifies the audio and video init
// probes are issued concurrently: with every upstream response delayed by
// 300ms, serial probes would first pipe media at ~900ms (probe+probe+batch)
// while parallel probes make it ~600ms (probe∥probe, then batch).
func TestDASHStream_InitProbesRunInParallel(t *testing.T) {
	const delay = 300 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"), strings.HasPrefix(r.URL.Path, "/video299/"):
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/audio140/"), "/video299/"), "sq/"), "/", 2)[0])
			time.Sleep(delay)
			if strings.HasPrefix(r.URL.Path, "/audio140/") {
				w.Write(mkBatch(1, []uint64{uint64(sq) * 1000}))
			} else {
				w.Write(mkBatch(1, []uint64{uint64(sq) * 20000}))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}

	start := time.Now()
	s := NewDASHStream(extractFn, ts.Client())
	defer s.Close()

	// Read until a moof followed by an mdat shows up; that is the earliest
	// observable proof that the merged init was piped and the first media
	// batch completed.
	firstMedia := make(chan time.Duration, 1)
	go func() {
		buf := make([]byte, 65536)
		var acc []byte
		for {
			n, err := s.Read(buf)
			acc = append(acc, buf[:n]...)
			sawMoof := false
			for _, b := range walkBoxes(acc, 0, len(acc)) {
				if b.typ == "moof" {
					sawMoof = true
				} else if b.typ == "mdat" && sawMoof {
					firstMedia <- time.Since(start)
					return
				}
			}
			if err != nil {
				firstMedia <- time.Hour
				return
			}
		}
	}()

	select {
	case d := <-firstMedia:
		// Parallel: ~2×delay (probe pair overlapped, then the batch pair
		// overlapped). Serial: ~3×delay. The 2.5×delay threshold separates
		// them with margin on both sides.
		if threshold := time.Duration(float64(delay) * 2.5); d > threshold {
			t.Errorf("first media after %s, probes look serial (threshold %s)", d, threshold)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no media produced within 5s")
	}
}

// TestDASHStream_ProactiveRenewal verifies that the BaseURLs are renewed
// before the ~30s session window lapses (YouTube 403s every request of a
// session after that) without resetting the tfdt cursors: the stream stays
// gapless with no duplicated moofs across renewals.
func TestDASHStream_ProactiveRenewal(t *testing.T) {
	tss := newTrackServer()
	var mpdReqs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			mpdReqs.Add(1)
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			tss.mu.Lock()
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			tfdts, ok := tss.audioSQ[sq]
			tss.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			tss.mu.Lock()
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/video299/sq/"), "/", 2)[0])
			tfdts, ok := tss.videoSQ[sq]
			tss.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(mkBatch(1, tfdts))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	var extractCount atomic.Int32
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		extractCount.Add(1)
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}

	s := NewDASHStream(extractFn, ts.Client())
	// sessionRenewInterval is 300ms in tests; run long enough for several
	// renewals plus re-extraction cycles triggered by exhausted sq data.
	data := readAllUntilClose(t, s, 2500*time.Millisecond)

	if extractCount.Load() < 3 {
		t.Fatalf("extractFn called %d times, want >= 3 (initial + renewals/re-extractions)", extractCount.Load())
	}
	if mpdReqs.Load() < 3 {
		t.Fatalf("MPD fetched %d times, want >= 3", mpdReqs.Load())
	}

	// The stream must stay well-formed across renewals: exactly one init
	// (ftyp+moov pair per re-extraction is fine, but they must be paired)
	// and no tfdt ever piped twice per track.
	boxes := walkBoxes(data, 0, len(data))
	ftypCount, moovCount := 0, 0
	seen := map[uint32]map[uint64]bool{}
	var lastByTrack map[uint32]uint64 = map[uint32]uint64{}
	for _, b := range boxes {
		switch b.typ {
		case "ftyp":
			ftypCount++
		case "moov":
			moovCount++
		case "moof":
			trackID := uint32(0)
			for _, traf := range walkBoxes(data, b.off+8, b.off+b.size) {
				if traf.typ != "traf" {
					continue
				}
				for _, tb := range walkBoxes(data, traf.off+8, traf.off+traf.size) {
					if tb.typ == "tfhd" {
						trackID = binary.BigEndian.Uint32(data[tb.off+8+4 : tb.off+8+8])
					}
				}
			}
			tfdt, ok := moofTfdt(data, b.off, b.size)
			if !ok {
				continue
			}
			if seen[trackID] == nil {
				seen[trackID] = map[uint64]bool{}
			}
			if seen[trackID][tfdt] {
				t.Errorf("tfdt %d on track %d piped twice across renewals", tfdt, trackID)
			}
			seen[trackID][tfdt] = true
			if prev, ok := lastByTrack[trackID]; ok && tfdt <= prev {
				t.Errorf("tfdt %d on track %d not increasing (prev %d)", tfdt, trackID, prev)
			}
			lastByTrack[trackID] = tfdt
		}
	}
	if ftypCount < 1 {
		t.Fatal("no init piped at all")
	}
	if moovCount != ftypCount {
		t.Errorf("ftyp=%d moov=%d, want paired", ftypCount, moovCount)
	}
}
