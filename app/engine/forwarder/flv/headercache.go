package flv

import (
	"sync"

	"github.com/nv4d1k/live-stream-forwarder/global"
)

// HeaderCache stores cached FLV headers keyed by "platform:room".
type HeaderCache struct {
	mu      sync.RWMutex
	entries map[string]*HeaderEntry
}

// HeaderEntry holds a cached FLV header and a readiness signal.
type HeaderEntry struct {
	mu    sync.RWMutex
	data  []byte
	ready chan struct{}
	once  sync.Once
}

// DefaultCache is the process-wide FLV header cache.
var DefaultCache = NewHeaderCache()

func NewHeaderCache() *HeaderCache {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.NewHeaderCache")
	log.Debug("creating HeaderCache")
	return &HeaderCache{
		entries: make(map[string]*HeaderEntry),
	}
}

// GetOrCreate returns the existing HeaderEntry for key, or creates a new one.
func (c *HeaderCache) GetOrCreate(key string) *HeaderEntry {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.GetOrCreate")
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		return e
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-check after acquiring write lock.
	if e, ok = c.entries[key]; ok {
		return e
	}
	e = newHeaderEntry()
	c.entries[key] = e
	log.WithField("key", key).Debug("created new header cache entry")
	return e
}

func newHeaderEntry() *HeaderEntry {
	return &HeaderEntry{
		ready: make(chan struct{}),
	}
}

// Set stores the FLV header data and signals readiness.
// May be called multiple times (on 403 reconnect with a fresh stream);
// each call updates the cached data.
func (e *HeaderEntry) Set(data []byte) {
	log := global.Log.WithField("func", "app.engine.forwarder.flv.Set")
	copied := make([]byte, len(data))
	copy(copied, data)

	e.mu.Lock()
	e.data = copied
	e.mu.Unlock()

	e.once.Do(func() { close(e.ready) })
	log.WithField("size", len(data)).Debug("cached FLV header")
}

// Wait blocks until the header data is available (Set has been called at least once).
func (e *HeaderEntry) Wait() {
	<-e.ready
}

// Data returns the cached header data. Caller should call Wait first to ensure
// data is available.
func (e *HeaderEntry) Data() []byte {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.data
}
