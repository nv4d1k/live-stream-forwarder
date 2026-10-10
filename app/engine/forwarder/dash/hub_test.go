package dash

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
)

// newFanoutServer serves an MPD plus unlimited-sq batches for both tracks
// (tfdt derived from sq, strictly increasing), counting MPD requests so
// tests can assert how many upstream sessions were opened.
func newFanoutServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var mpdReqs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			mpdReqs.Add(1)
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
	t.Cleanup(ts.Close)
	tsServerBase = ts.URL
	return ts, &mpdReqs
}

// readFor reads everything the reader produces within d, then closes it.
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

// firstMoofOffset returns the offset of the first moof box, i.e. the length
// of the init part.
func firstMoofOffset(t *testing.T, data []byte) int {
	t.Helper()
	for _, b := range walkBoxes(data, 0, len(data)) {
		if b.typ == "moof" {
			return b.off
		}
	}
	t.Fatalf("no moof in %d bytes of data", len(data))
	return 0
}

func TestHub_GetOrCreateShares(t *testing.T) {
	hub := &Hub{streams: map[string]*SharedStream{}}
	calls := 0
	mk := func() *DASHStream {
		calls++
		return &DASHStream{pipe: stream.NewPipe(), done: make(chan struct{})}
	}
	a := hub.GetOrCreate("k", mk)
	b := hub.GetOrCreate("k", mk)
	if a != b {
		t.Error("same key must return the same SharedStream")
	}
	c := hub.GetOrCreate("other", mk)
	if c == a {
		t.Error("different key must return a different SharedStream")
	}
	if calls != 2 {
		t.Errorf("core created %d times, want 2 (one per key)", calls)
	}
	if n := hub.Len(); n != 2 {
		t.Errorf("hub size = %d, want 2", n)
	}
	// Teardown removes only the matching instance.
	hub.remove("k", c) // wrong instance for key "k"
	if n := hub.Len(); n != 2 {
		t.Errorf("hub size after removing wrong instance = %d, want 2", n)
	}
	hub.remove("k", a)
	if n := hub.Len(); n != 1 {
		t.Errorf("hub size after teardown = %d, want 1", n)
	}
}

// TestSharedStream_FanOutSharedUpstream verifies the core property of the
// fan-out: several subscribers get complete streams (merged init first) from
// a single core producer.
func TestSharedStream_FanOutSharedUpstream(t *testing.T) {
	ts, _ := newFanoutServer(t)
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	var cores atomic.Int32
	ss := hub.GetOrCreate("yt:room:proxy", func() *DASHStream {
		cores.Add(1)
		return NewDASHStream(extractFn, ts.Client())
	})

	sub1 := ss.Subscribe()
	data1 := readFor(t, sub1, 700*time.Millisecond)
	if len(data1) == 0 {
		t.Fatal("first subscriber got no data")
	}
	initLen := firstMoofOffset(t, data1)
	if !bytes.HasPrefix(data1, []byte{0, 0, 0}) {
		t.Fatal("stream does not start with a box header")
	}

	sub2 := ss.Subscribe()
	data2 := readFor(t, sub2, 700*time.Millisecond)

	for name, d := range map[string][]byte{"sub1": data1, "sub2": data2} {
		if len(d) == 0 {
			t.Errorf("%s got no data", name)
			continue
		}
		if off := firstMoofOffset(t, d); off != initLen {
			t.Errorf("%s init length = %d, want %d (merged init replay)", name, off, initLen)
		}
		if n := len(walkBoxes(d, 0, len(d))); n < 4 {
			t.Errorf("%s produced only %d boxes, want a real stream", name, n)
		}
	}

	// The whole point: both subscribers were served by one core producer.
	if n := cores.Load(); n != 1 {
		t.Errorf("core created %d times for 2 subscribers, want 1 (fan-out broken)", n)
	}
}

// TestSharedStream_LateSubscriberReplay verifies a subscriber joining mid
// stream starts with the merged init followed by the buffered groups, and
// that those groups byte-match the ones the earlier subscriber received.
func TestSharedStream_LateSubscriberReplay(t *testing.T) {
	ts, _ := newFanoutServer(t)
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, ts.Client())
	})

	sub1 := ss.Subscribe()
	data1 := readFor(t, sub1, 800*time.Millisecond) // fill the replay buffer

	sub2 := ss.Subscribe()
	data2 := readFor(t, sub2, 300*time.Millisecond)

	// sub2 = init + replayed groups + live groups. sub1 ends at some group
	// boundary inside sub2's media: the first group sub2 received must be
	// found in sub1, and from there on both must be byte-identical up to
	// the end of data1.
	init2 := firstMoofOffset(t, data2)
	media2 := data2[init2:]
	if len(media2) == 0 {
		t.Fatal("late subscriber got init but no media groups")
	}
	// First group in sub2's media: up to (excluding) its second moof.
	firstGroupEnd := len(media2)
	moofSeen := 0
	for _, b := range walkBoxes(data2, init2, len(data2)) {
		if b.typ == "moof" {
			moofSeen++
			if moofSeen == 2 {
				firstGroupEnd = b.off - init2
				break
			}
		}
	}
	if firstGroupEnd == len(media2) {
		t.Fatalf("late subscriber media holds a single group only")
	}
	firstGroup := media2[:firstGroupEnd]
	idx := bytes.LastIndex(data1, firstGroup)
	if idx < 0 {
		t.Fatalf("late subscriber's first replayed group not found in the live stream tail")
	}
	if !bytes.Equal(data1[idx:], media2[:len(data1)-idx]) {
		t.Errorf("replayed groups diverge from the live stream (align at %d, sub1 tail %d bytes vs sub2 head)", idx, len(data1)-idx)
	}
}

// TestSharedStream_IdleTeardown verifies that after the last subscriber
// leaves, the shared stream is torn down (upstream producer closed, hub
// entry removed) once the grace period lapses.
func TestSharedStream_IdleTeardown(t *testing.T) {
	ts, mpdReqs := newFanoutServer(t)
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()
	hub.IdleGrace = 200 * time.Millisecond
	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, ts.Client())
	})

	sub := ss.Subscribe()
	_ = readFor(t, sub, 400*time.Millisecond)
	if n := hub.Len(); n != 1 {
		t.Fatalf("hub size before teardown = %d, want 1", n)
	}

	// Idle grace lapses with no subscribers: entry must disappear and the
	// upstream session must stop renewing (MPD requests frozen).
	time.Sleep(700 * time.Millisecond)
	if n := hub.Len(); n != 0 {
		t.Errorf("hub size after idle grace = %d, want 0", n)
	}
	late := mpdReqs.Load()
	time.Sleep(400 * time.Millisecond)
	if after := mpdReqs.Load(); after != late {
		t.Errorf("MPD still fetched after teardown (%d -> %d), producer not stopped", late, after)
	}
}

// TestSharedStream_ProducerEndClosesSubs verifies that when the core stream
// dies with a terminal error (format deadlock), every subscriber sees the
// error and the hub entry is removed.
func TestSharedStream_ProducerEndClosesSubs(t *testing.T) {
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return nil, fmt.Errorf("room moved to p2p: %w", stream.ErrFormatDeadlock)
	}
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, http.DefaultClient)
	})
	sub1 := ss.Subscribe()
	sub2 := ss.Subscribe()

	deadline := time.Now().Add(5 * time.Second)
	var err1, err2 error
	readErr := func(r io.ReadCloser) error {
		_, err := io.ReadAll(r)
		return err
	}
	ch1, ch2 := make(chan error, 1), make(chan error, 1)
	go func() { ch1 <- readErr(sub1) }()
	go func() { ch2 <- readErr(sub2) }()
	select {
	case err1 = <-ch1:
	case <-time.After(time.Until(deadline)):
		t.Fatal("subscriber 1 stream never ended")
	}
	select {
	case err2 = <-ch2:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber 2 stream never ended")
	}
	if err1 == nil || err2 == nil {
		t.Errorf("subscribers must see an error, got %v / %v", err1, err2)
	}
	if !errors.Is(err1, stream.ErrFormatDeadlock) {
		t.Errorf("subscriber 1 error = %v, want ErrFormatDeadlock", err1)
	}
	if n := hub.Len(); n != 0 {
		t.Errorf("hub size after producer death = %d, want 0", n)
	}
}

// TestHub_SubscribeExisting verifies the fast path used by the HTTP layer:
// it returns a reader only when a shared stream already exists for the key,
// so a second player joins without paying a fresh extraction round trip.
func TestHub_SubscribeExisting(t *testing.T) {
	ts, _ := newFanoutServer(t)
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()

	// No stream yet: must return nil so the caller falls back to a full
	// extraction + dispatch.
	if r := hub.SubscribeExisting("k"); r != nil {
		r.Close()
		t.Fatal("SubscribeExisting must return nil when no stream exists")
	}

	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, ts.Client())
	})
	r := hub.SubscribeExisting("k")
	if r == nil {
		t.Fatal("SubscribeExisting returned nil for a live stream")
	}
	data := readFor(t, r, 500*time.Millisecond)
	if len(data) == 0 {
		t.Error("reader from SubscribeExisting produced no data")
	}

	if r := hub.SubscribeExisting("missing"); r != nil {
		r.Close()
		t.Error("SubscribeExisting must return nil for an unknown key")
	}
	_ = ss
}

// TestSharedStream_PublishesGroupWithoutWaitingForOtherTrack verifies the
// pump publishes a finished group (moof + mdat) as soon as its mdat
// arrives: with the video track delayed by 400ms, the audio group must
// reach subscribers immediately, not held back waiting for the video
// moof that follows it in the interleaved stream. This mirrors live rooms
// where the high-bitrate video batch lands many seconds after audio.
func TestSharedStream_PublishesGroupWithoutWaitingForOtherTrack(t *testing.T) {
	const videoDelay = 400 * time.Millisecond
	var videoReqs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mpd":
			w.Write(testMPD(strings.TrimSuffix(tsServerBase, "/")))
		case strings.HasPrefix(r.URL.Path, "/audio140/"):
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/audio140/sq/"), "/", 2)[0])
			w.Write(mkBatch(1, []uint64{uint64(sq) * 1000}))
		case strings.HasPrefix(r.URL.Path, "/video299/"):
			sq, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(r.URL.Path, "/video299/sq/"), "/", 2)[0])
			// The first video request is the init probe (must stay fast so
			// the merged init is piped); every later one is a media batch,
			// which stands in for a slow high-bitrate video segment.
			if videoReqs.Add(1) >= 2 {
				time.Sleep(videoDelay)
			}
			w.Write(mkBatch(1, []uint64{uint64(sq) * 20000}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	tsServerBase = ts.URL

	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, ts.Client())
	})

	// Read with a deadline shorter than the video delay: by then the
	// merged init and the first audio group must have been published.
	sub := ss.Subscribe()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var acc []byte
		buf := make([]byte, 65536)
		for {
			n, err := sub.Read(buf)
			acc = append(acc, buf[:n]...)
			if err != nil {
				ch <- result{acc, err}
				return
			}
			// Stop once a full moof+mdat pair is past the init.
			sawMoof, sawMdat := false, false
			for _, b := range walkBoxes(acc, 0, len(acc)) {
				switch b.typ {
				case "moof":
					sawMoof = true
				case "mdat":
					sawMdat = true
				}
			}
			if sawMoof && sawMdat {
				ch <- result{acc, nil}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read error before first group: %v", r.err)
		}
		if len(r.data) == 0 {
			t.Fatal("no data")
		}
	case <-time.After(videoDelay - 50*time.Millisecond):
		t.Fatal("audio group was held back waiting for the slow video track")
	}
	sub.Close()
}

// TestSharedStream_SlowSubscriberKicked verifies a subscriber that stops
// reading gets its subscription cut once its backlog exceeds the kick
// threshold, without affecting healthy subscribers.
func TestSharedStream_SlowSubscriberKicked(t *testing.T) {
	ts, _ := newFanoutServer(t)
	extractFn := func(*stream.ExtractResult) (*stream.ExtractResult, error) {
		return &stream.ExtractResult{URL: ts.URL + "/mpd"}, nil
	}
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	hub.KickBytes = 4096
	ss := hub.GetOrCreate("k", func() *DASHStream {
		return NewDASHStream(extractFn, ts.Client())
	})

	slow := ss.Subscribe()
	// Never read from slow; let the producer push data well past the kick
	// threshold.
	time.Sleep(1500 * time.Millisecond)

	// The kick closes the pipe with CloseWithError: the already-buffered
	// data stays readable and the error surfaces once it is drained. Keep
	// reading until then.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	var kickErr error
	for time.Now().Before(deadline) {
		_, err := slow.Read(buf)
		if err != nil {
			kickErr = err
			break
		}
	}
	if kickErr == nil {
		t.Error("slow subscriber should have been kicked, stream never ended")
	} else if !errors.Is(kickErr, errSlowSubscriber) {
		t.Errorf("slow subscriber ended with %v, want errSlowSubscriber", kickErr)
	}

	fast := ss.Subscribe()
	data := readFor(t, fast, 400*time.Millisecond)
	if n := len(walkBoxes(data, 0, len(data))); n < 4 {
		t.Errorf("healthy subscriber broke after kick: %d bytes, %d boxes", len(data), n)
	}
}
