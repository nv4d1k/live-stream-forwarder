package dash

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/stream"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

// Fan-out: one upstream DASHStream (the "core") per stream key, shared by
// any number of client subscribers. The core is the only connection to
// YouTube — one innertube session, one pair of segment fetches per cycle,
// regardless of how many players are watching. A pump goroutine reads the
// core's interleaved stream box by box, keeps a ring buffer of the merged
// init plus the most recent moof groups, and replays that snapshot to every
// subscriber on join, then tails it with live groups.
//
// Subscribers that stop reading are cut once their backlog exceeds
// Hub.KickBytes (a live stream must not buffer unboundedly for a dead
// client); they simply reconnect and rejoin from the live edge.

// errSlowSubscriber terminates the stream of a subscriber whose backlog
// grew past the hub's KickBytes.
var errSlowSubscriber = errors.New("dash: subscriber fell too far behind the live edge")

// Hub routes stream keys to shared DASH streams. The tuning fields are read
// once when a stream is created, so they may be adjusted on a fresh hub but
// must not be modified while streams are live. Use NewHub or DefaultHub.
type Hub struct {
	mu      sync.Mutex
	streams map[string]*SharedStream

	// IdleGrace is how long a shared stream survives without subscribers
	// before its core producer is torn down. Player probe requests (open,
	// close, reopen) land within it, so a stream is not rebuilt between the
	// probe and the real playback.
	IdleGrace time.Duration

	// KickBytes is the subscriber pipe backlog above which the subscriber
	// is considered dead and gets cut.
	KickBytes int

	// ReplayMaxGroups / ReplayMaxBytes bound the ring buffer a new
	// subscriber replays from: the most recent moof groups, capped by count
	// and total size (~16 groups ≈ 16s at YouTube's ~2s segments).
	ReplayMaxGroups int
	ReplayMaxBytes  int
}

// NewHub creates a hub with the default tuning.
func NewHub() *Hub {
	return &Hub{
		streams:         map[string]*SharedStream{},
		IdleGrace:       30 * time.Second,
		KickBytes:       48 << 20,
		ReplayMaxGroups: 16,
		ReplayMaxBytes:  32 << 20,
	}
}

// DefaultHub is the process-wide hub used by the HTTP layer.
var DefaultHub = NewHub()

// GetOrCreate returns the shared stream for key, creating (and starting) a
// new core producer on first use. create is only invoked when no stream
// exists for the key.
func (h *Hub) GetOrCreate(key string, create func() *DASHStream) *SharedStream {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.Hub.GetOrCreate")
	h.mu.Lock()
	defer h.mu.Unlock()
	if ss, ok := h.streams[key]; ok {
		log.WithField("key", key).Debug("reusing shared DASH stream")
		return ss
	}
	ss := newSharedStream(h, key, create())
	h.streams[key] = ss
	log.WithField("key", key).Info("created shared DASH stream")
	return ss
}

// SubscribeExisting returns a reader for the shared stream under key, or
// nil when no live stream exists for it. It is the HTTP layer's fast path:
// a second player joining a room already being forwarded skips the whole
// extraction round trip (innertube + MPD + init probes) and immediately
// replays the buffered init and groups.
func (h *Hub) SubscribeExisting(key string) io.ReadCloser {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.Hub.SubscribeExisting")
	h.mu.Lock()
	ss, ok := h.streams[key]
	h.mu.Unlock()
	if !ok {
		return nil
	}
	log.WithField("key", key).Debug("fast-path subscriber attached")
	return ss.Subscribe()
}

// Len reports the number of live shared streams.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

// remove drops the stream for key, but only if ss is still the registered
// instance (a newer stream may have replaced it after a teardown).
func (h *Hub) remove(key string, ss *SharedStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.streams[key]; ok && cur == ss {
		delete(h.streams, key)
	}
}

// SharedStream fans one core DASHStream out to many subscribers.
type SharedStream struct {
	hub  *Hub
	key  string
	core *DASHStream

	// Tuning snapshotted from the hub at creation time.
	idleGrace       time.Duration
	kickBytes       int
	replayMaxGroups int
	replayMaxBytes  int

	mu        sync.Mutex
	subs      map[*subscriber]struct{}
	init      []byte   // merged init; set once the core piped it
	groups    [][]byte // ring buffer of recent moof groups
	total     int      // total bytes held in groups
	idleTimer *time.Timer

	teardownOnce sync.Once
}

// newSharedStream builds a SharedStream around core and starts its pump.
// The idle timer is armed immediately so a stream that is never subscribed
// to is still reaped.
func newSharedStream(hub *Hub, key string, core *DASHStream) *SharedStream {
	ss := &SharedStream{
		hub:             hub,
		key:             key,
		core:            core,
		idleGrace:       hub.IdleGrace,
		kickBytes:       hub.KickBytes,
		replayMaxGroups: hub.ReplayMaxGroups,
		replayMaxBytes:  hub.ReplayMaxBytes,
		subs:            map[*subscriber]struct{}{},
	}
	ss.mu.Lock()
	ss.idleTimer = time.AfterFunc(ss.idleGrace, ss.idleExpired)
	ss.mu.Unlock()
	go ss.pump()
	return ss
}

// Subscribe attaches a new client reader. It immediately replays the merged
// init and the buffered groups (when already produced), then tails the live
// stream. Closing the reader detaches the client; when the last client
// leaves, the core is torn down after idleGrace.
func (s *SharedStream) Subscribe() io.ReadCloser {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.Subscribe")
	s.mu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	sub := &subscriber{ss: s, pipe: stream.NewPipe()}
	if s.init != nil {
		// Replay under the lock so no live group can interleave before the
		// snapshot is fully written (publish takes the same lock).
		sub.pipe.Write(s.init)
		for _, g := range s.groups {
			sub.pipe.Write(g)
		}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	log.WithField("key", s.key).Debug("subscriber attached")
	return &subReader{sub: sub}
}

// unsubscribe detaches a subscriber (client disconnect or kicked); it arms
// the idle timer when the last subscriber left.
func (s *SharedStream) unsubscribe(sub *subscriber) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.unsubscribe")
	s.mu.Lock()
	delete(s.subs, sub)
	if len(s.subs) == 0 && s.idleTimer == nil {
		s.idleTimer = time.AfterFunc(s.idleGrace, s.idleExpired)
	}
	s.mu.Unlock()
	sub.pipe.BreakWithError(io.ErrClosedPipe)
	log.WithField("key", s.key).Debug("subscriber detached")
}

// idleExpired tears the stream down when the grace period lapsed with no
// subscribers.
func (s *SharedStream) idleExpired() {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.idleExpired")
	s.mu.Lock()
	empty := len(s.subs) == 0
	s.idleTimer = nil
	s.mu.Unlock()
	if empty {
		log.WithField("key", s.key).Info("shared stream idle, tearing down")
		s.teardown(io.ErrClosedPipe)
	}
}

// publishInit stores the merged init and delivers it to every subscriber
// that attached before the core produced it.
func (s *SharedStream) publishInit(init []byte) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.publishInit")
	s.mu.Lock()
	s.init = init
	for sub := range s.subs {
		sub.pipe.Write(init)
	}
	s.mu.Unlock()
	log.WithField("key", s.key).Debugf("captured merged init (%d bytes)", len(init))
}

// publish appends a moof group to the ring buffer and broadcasts it to all
// subscribers. The group slice is owned by the ring buffer from here on —
// the pump must hand over a freshly allocated buffer per group. Dead pipes
// and backlogs beyond the kick threshold are cut.
func (s *SharedStream) publish(group []byte) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.publish")
	s.mu.Lock()
	s.groups = append(s.groups, group)
	s.total += len(group)
	for len(s.groups) > 0 && (s.total > s.replayMaxBytes || len(s.groups) > s.replayMaxGroups) {
		s.total -= len(s.groups[0])
		copy(s.groups, s.groups[1:])
		s.groups = s.groups[:len(s.groups)-1]
	}
	var dead []*subscriber
	for sub := range s.subs {
		if _, err := sub.pipe.Write(group); err != nil {
			dead = append(dead, sub)
			continue
		}
		if sub.pipe.Len() > s.kickBytes {
			sub.pipe.CloseWithError(errSlowSubscriber)
			dead = append(dead, sub)
		}
	}
	for _, sub := range dead {
		delete(s.subs, sub)
	}
	if len(s.subs) == 0 && len(dead) > 0 && s.idleTimer == nil {
		// The last subscriber was kicked; no unsubscribe will fire.
		s.idleTimer = time.AfterFunc(s.idleGrace, s.idleExpired)
	}
	s.mu.Unlock()
	if len(dead) > 0 {
		log.WithField("key", s.key).Warnf("cut %d subscriber(s) (closed or too slow)", len(dead))
	}
}

// pump reads the core stream box by box, captures the merged init (all
// boxes before the first moof) and republishes each moof group (a moof plus
// the boxes up to the next moof) to the subscribers. It exits on any core
// error, tearing the shared stream down.
func (s *SharedStream) pump() {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.pump")
	br := newBoxReader(s.core)
	var initBuf []byte
	var group []byte
	initPublished := false
	for {
		box, typ, err := br.next()
		if err != nil {
			s.teardown(fmt.Errorf("core stream ended: %w", err))
			return
		}
		switch {
		case typ == "moof":
			if !initPublished {
				s.publishInit(initBuf)
				initPublished = true
			}
			if len(group) > 0 {
				// A moof while a group is open can only come from a
				// malformed core stream (groups are moof+mdat pairs);
				// flush what is there rather than concatenating groups.
				log.WithField("key", s.key).Warn("moof arrived with an open group, flushing it")
				s.publish(group)
			}
			group = append(group, box...)
		case typ == "mdat":
			// A batch is init + (moof + mdat)*: the mdat completes the
			// group, so publish immediately. The other track's next group
			// can be seconds out (high-bitrate video batches), and holding
			// a finished group back for it would starve every subscriber.
			if len(group) == 0 {
				log.WithField("key", s.key).Warn("orphan mdat without a moof, dropping it")
				continue
			}
			group = append(group, box...)
			s.publish(group)
			// group is now owned by the ring buffer; start a fresh buffer
			// instead of reusing its backing array.
			group = nil
		case len(group) > 0:
			// Boxes between a group's moof and its mdat (not produced by
			// the core today) join the group.
			group = append(group, box...)
		case !initPublished:
			// ftyp/moov/emsg of the merged init.
			initBuf = append(initBuf, box...)
		default:
			// Boxes outside any group after the init (not produced by the
			// core today) are dropped rather than corrupting the stream.
			log.WithField("key", s.key).Warnf("unexpected %s box outside init and groups", typ)
		}
	}
}

// teardown stops the core producer, closes every subscriber with err and
// removes the stream from its hub. Idempotent.
func (s *SharedStream) teardown(err error) {
	log := global.Log.WithField("func", "app.engine.forwarder.dash.SharedStream.teardown")
	s.teardownOnce.Do(func() {
		log.WithField("key", s.key).Warnf("tearing down shared stream: %s", err.Error())
		s.core.Close()
		s.mu.Lock()
		subs := s.subs
		s.subs = map[*subscriber]struct{}{}
		if s.idleTimer != nil {
			s.idleTimer.Stop()
			s.idleTimer = nil
		}
		s.mu.Unlock()
		for sub := range subs {
			sub.pipe.CloseWithError(err)
		}
		s.hub.remove(s.key, s)
	})
}

// subscriber is one attached client.
type subscriber struct {
	ss   *SharedStream
	pipe *stream.Pipe
}

// subReader exposes a subscriber as an io.ReadCloser. Close detaches the
// client from the shared stream.
type subReader struct {
	sub  *subscriber
	once sync.Once
}

func (r *subReader) Read(p []byte) (int, error) {
	return r.sub.pipe.Read(p)
}

func (r *subReader) Close() error {
	r.once.Do(func() {
		r.sub.ss.unsubscribe(r.sub)
	})
	return nil
}
