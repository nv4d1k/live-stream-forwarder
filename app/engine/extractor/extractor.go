package extractor

import (
	"net/http"
	"net/url"
)

// Result holds the resolved stream URL and any headers required to fetch it.
type Result struct {
	URL     string
	Headers http.Header
}

// Extractor is the unified interface that every platform extractor must implement.
type Extractor interface {
	// Extract resolves a stream URL for the given format and returns a Result
	// containing the URL and any required headers. The format string is a
	// platform-specific hint such as "flv", "m3u8", or "hls". If format is
	// empty, the extractor should use its DefaultFormat().
	Extract(format string) (*Result, error)

	// SupportedFormats returns the list of stream format identifiers this
	// extractor can produce (e.g. ["flv", "m3u8"]).
	SupportedFormats() []string

	// DefaultFormat returns the format to use when the caller does not specify
	// one.
	DefaultFormat() string
}

// Factory creates an Extractor for a given room ID and optional proxy.
type Factory func(rid string, proxy *url.URL) (Extractor, error)

// RegistryEntry bundles a Factory with platform-specific forwarding config.
type RegistryEntry struct {
	Factory      Factory
	Mobile       bool // whether to use mobile User-Agent for HTTP transport
	InitialError int  // HTTP status code for initial extraction errors
}

// Registry maps lowercase platform names to their entries.
var Registry = map[string]RegistryEntry{}

// Register adds a platform to the Registry. Called from each platform
// package's init().
func Register(platform string, entry RegistryEntry) {
	Registry[platform] = entry
}
