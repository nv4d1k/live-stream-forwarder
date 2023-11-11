package websocket

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// Pipe is a goroutine-safe io.Reader/io.Writer pair. It's like
// io.Pipe except there are no PipeReader/PipeWriter halves, and the
// underlying buffer is an interface. (io.Pipe is always unbuffered)
type Pipe struct {
	mu       sync.Mutex
	c        sync.Cond     // c.L lazily initialized to &p.mu
	b        pipeBuffer    // nil when done reading
	unread   int           // bytes unread when done
	err      error         // read error once empty. non-nil means closed.
	breakErr error         // immediate read error (caller doesn't see rest of b)
	donec    chan struct{} // closed on error
	readFn   func()        // optional code to run in Read before error
}

type pipeBuffer interface {
	Len() int
	io.Writer
	io.Reader
}

func NewPipe() *Pipe {
	p := &Pipe{}
	p.setBuffer(bytes.NewBuffer(nil))
	return p
}

// setBuffer initializes the pipe buffer.
// It has no effect if the pipe is already closed.
func (p *Pipe) setBuffer(b pipeBuffer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil || p.breakErr != nil {
		return
	}
	p.b = b
}

func (p *Pipe) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.b == nil {
		return p.unread
	}
	return p.b.Len()
}

// Read waits until data is available and copies bytes
// from the buffer into p.
func (p *Pipe) Read(d []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c.L == nil {
		p.c.L = &p.mu
	}
	for {
		if p.breakErr != nil {
			return 0, p.breakErr
		}
		if p.b != nil && p.b.Len() > 0 {
			return p.b.Read(d)
		}
		if p.err != nil {
			if p.readFn != nil {
				p.readFn()     // e.g. copy trailers
				p.readFn = nil // not sticky like p.err
			}
			p.b = nil
			return 0, p.err
		}
		p.c.Wait()
	}
}

var errClosedPipeWrite = errors.New("write on closed buffer")

// Write copies bytes from p into the buffer and wakes a reader.
// It is an error to write more data than the buffer can hold.
func (p *Pipe) Write(d []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c.L == nil {
		p.c.L = &p.mu
	}
	defer p.c.Signal()
	if p.err != nil || p.breakErr != nil {
		return 0, errClosedPipeWrite
	}
	return p.b.Write(d)
}

// CloseWithError causes the next Read (waking up a current blocked
// Read if needed) to return the provided err after all data has been
// read.
//
// The error must be non-nil.
func (p *Pipe) CloseWithError(err error) { p.closeWithError(&p.err, err, nil) }

// BreakWithError causes the next Read (waking up a current blocked
// Read if needed) to return the provided err immediately, without
// waiting for unread data.
func (p *Pipe) BreakWithError(err error) { p.closeWithError(&p.breakErr, err, nil) }

// closeWithErrorAndCode is like CloseWithError but also sets some code to run
// in the caller's goroutine before returning the error.
func (p *Pipe) closeWithErrorAndCode(err error, fn func()) { p.closeWithError(&p.err, err, fn) }

func (p *Pipe) closeWithError(dst *error, err error, fn func()) {
	if err == nil {
		panic("err must be non-nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c.L == nil {
		p.c.L = &p.mu
	}
	defer p.c.Signal()
	if *dst != nil {
		// Already been done.
		return
	}
	p.readFn = fn
	if dst == &p.breakErr {
		if p.b != nil {
			p.unread += p.b.Len()
		}
		p.b = nil
	}
	*dst = err
	p.closeDoneLocked()
}

// requires p.mu be held.
func (p *Pipe) closeDoneLocked() {
	if p.donec == nil {
		return
	}
	// Close if unclosed. This isn't racy since we always
	// hold p.mu while closing.
	select {
	case <-p.donec:
	default:
		close(p.donec)
	}
}

// Err returns the error (if any) first set by BreakWithError or CloseWithError.
func (p *Pipe) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.breakErr != nil {
		return p.breakErr
	}
	return p.err
}

// Done returns a channel which is closed if and when this pipe is closed
// with CloseWithError.
func (p *Pipe) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.donec == nil {
		p.donec = make(chan struct{})
		if p.err != nil || p.breakErr != nil {
			// Already hit an error.
			p.closeDoneLocked()
		}
	}
	return p.donec
}
