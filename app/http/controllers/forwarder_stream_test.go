package controllers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// neverReader never produces data: Read parks until the test unblocks it,
// mimicking a forwarder whose upstream is mid-download (e.g. a DASH video
// batch crawling through a proxy) so nothing reaches the client for a long
// stretch.
type neverReader struct {
	block   chan struct{}
	onClose func()
}

func (r *neverReader) Read(p []byte) (int, error) {
	<-r.block
	return 0, nil
}

func (r *neverReader) Close() error {
	if r.onClose != nil {
		r.onClose()
	}
	return nil
}

// TestStreamToClient_DetectsClientDisconnect verifies the copy loop notices
// the client walking away even while blocked waiting for stream data: the
// reader must be Closed promptly so subscriber cleanup (dash hub reference
// counting, idle teardown) can run. Blocking in Read alone would leak the
// reader until the next data write fails — for a stalled upstream that is
// potentially never, and a leaked dash subscriber means the shared core is
// never torn down and keeps fetching upstream data with nobody watching.
func TestStreamToClient_DetectsClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := &neverReader{block: make(chan struct{})}
	closed := make(chan struct{})
	r.onClose = func() { close(closed) }

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("GET", "/stream", nil)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = req.WithContext(ctx)

	go streamToClient(c, r, "video/mp4")

	// Give the copy loop a moment to park inside Read, then walk away
	// without any data ever being delivered.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-closed:
		// Green path; release the reader goroutine if it is still parked.
		select {
		case <-r.block:
		default:
			close(r.block)
		}
	case <-time.After(3 * time.Second):
		// Unblock the parked reader so the copy goroutine can exit, then
		// fail: the reader must not depend on data to notice a disconnect.
		close(r.block)
		t.Fatal("reader was not closed within 3s of the client disconnecting")
	}
}
