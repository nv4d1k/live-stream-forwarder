package fanout

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	m.Run()
}

// fakeCore is a minimal io.ReadCloser core: it hands out its payload once
// (blocking reads like a live stream until closed) and records Close calls.
type fakeCore struct {
	mu       sync.Mutex
	payload  []byte
	closed   chan struct{}
	closeOne sync.Once
}

func newFakeCore(payload []byte) *fakeCore {
	return &fakeCore{payload: payload, closed: make(chan struct{})}
}

func (c *fakeCore) Read(p []byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *fakeCore) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}

func TestHub_GetOrCreateShares(t *testing.T) {
	hub := &Hub{streams: map[string]*SharedStream{}}
	calls := 0
	mk := func() io.ReadCloser {
		calls++
		return newFakeCore(nil)
	}
	a := hub.GetOrCreate("k", mk, FramingBMFF)
	b := hub.GetOrCreate("k", mk, FramingBMFF)
	if a != b {
		t.Error("same key must return the same SharedStream")
	}
	c := hub.GetOrCreate("other", mk, FramingBMFF)
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

// TestSharedStream_CoreCloseClosesSubscribers verifies that a core dying
// mid-stream tears the shared stream down and surfaces the error to every
// subscriber.
func TestSharedStream_CoreCloseClosesSubscribers(t *testing.T) {
	hub := NewHub()
	hub.IdleGrace = 10 * time.Second
	ss := hub.GetOrCreate("k", func() io.ReadCloser { return newFakeCore(nil) }, FramingBMFF)
	sub := ss.Subscribe()
	errCh := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(sub)
		errCh <- err
	}()
	// Kill the core; the pump must tear down and close the subscriber.
	ss.core.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("subscriber stream ended cleanly, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never ended after core death")
	}
	if n := hub.Len(); n != 0 {
		t.Errorf("hub size after core death = %d, want 0", n)
	}
}
