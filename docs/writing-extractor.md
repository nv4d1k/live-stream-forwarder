# Writing a Custom Extractor

This guide walks through creating a new platform extractor for Live Stream Forwarder. An extractor resolves a room ID into a playable stream URL.

## Overview

The extractor system uses a **registry pattern**: each platform self-registers via `init()`, and the controller looks up platforms by name from `extractor.Registry`. Adding a new platform requires no changes to the controller or any existing code.

## Extractor Interface

Every extractor must implement the `Extractor` interface defined in `app/engine/extractor/extractor.go`:

```go
type Extractor interface {
    Extract(format string) (*Result, error)
    SupportedFormats() []string
    DefaultFormat() string
}
```

| Method | Description |
|--------|-------------|
| `Extract(format)` | Resolves a stream URL for the given format. Returns a `Result` containing the URL and any required HTTP headers. If `format` is empty, use `DefaultFormat()`. |
| `SupportedFormats()` | Returns the list of format identifiers this extractor can produce (e.g. `["flv", "m3u8"]`). |
| `DefaultFormat()` | Returns the format to use when the caller does not specify one. |

The `Result` struct:

```go
type Result struct {
    URL     string
    Headers http.Header
}
```

Set `Headers` when the upstream server requires specific headers (e.g. `Referer`, `Cookie`). The forwarder will include these headers when fetching the stream.

## Step-by-Step Guide

### 1. Create the package directory

```
app/engine/extractor/MyPlatform/
```

The directory name becomes the URL token — users access streams at `http://localhost:8080/myplatform/<room>` (lowercased automatically).

### 2. Implement the extractor

Create the main file (e.g. `myplatform.go`) with the struct, `init()`, constructor, and interface methods:

```go
package MyPlatform

import (
    "net/http"
    "net/url"

    "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
    "github.com/nv4d1k/live-stream-forwarder/app/engine/forwarder/httpweb"
    "github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
    if global.Log != nil {
        log := global.Log.WithField("func", "app.engine.extractor.MyPlatform.init")
        log.Infoln("registering extractor")
    }
    extractor.Register("myplatform", extractor.RegistryEntry{
        Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
            return NewLink(rid, proxy)
        },
        Mobile:       false,
        InitialError: 500,
    })
}

type Link struct {
    rid    string
    client *http.Client
}

func NewLink(rid string, proxy *url.URL) (*Link, error) {
    log := global.Log.WithField("func", "app.engine.extractor.MyPlatform.NewLink")
    l := &Link{rid: rid}
    if proxy != nil {
        l.client = &http.Client{
            Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false),
        }
    } else {
        l.client = &http.Client{
            Transport: httpweb.NewAddHeaderTransport(nil, false),
        }
    }
    // Perform any initialization here (e.g. fetch room info, obtain tokens)
    log.Infof("extractor created for room %s", rid)
    return l, nil
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
    log := global.Log.WithField("func", "app.engine.extractor.MyPlatform.Extract")
    if format == "" {
        format = l.DefaultFormat()
    }
    u, err := l.GetLink(format)
    if err != nil {
        log.Errorf("failed to get link for room %s: %v", l.rid, err)
        return nil, err
    }
    log.Debugf("extracted stream URL for room %s", l.rid)
    return &extractor.Result{URL: u}, nil
}

func (l *Link) SupportedFormats() []string {
    return []string{"flv", "m3u8"}
}

func (l *Link) DefaultFormat() string {
    return "flv"
}
```

### 3. Implement the extraction logic

Place the actual stream URL resolution logic in a separate file (e.g. `extract.go`):

```go
package MyPlatform

func (l *Link) GetLink(format string) (string, error) {
    // 1. Call the platform's API to get the stream URL
    // 2. Return the URL string
    // 3. If headers are required, return them via the Result struct
}
```

### 4. Register the blank import

Add a blank import in `app/http/controllers/forwarder.go` to trigger the `init()` function:

```go
import (
    // ... existing imports ...
    _ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/MyPlatform"
)
```

That's it — no other code changes are needed. The controller will automatically route requests to `/<platform>/<room>` to your extractor.

## Conventions

### Logging

Use `global.Log` with a `"func"` field in every function. The format is the dot-separated path from the project root to the function:

```go
log := global.Log.WithField("func", "app.engine.extractor.MyPlatform.Extract")
```

Add logging at appropriate levels:
- **Info**: constructor creation, significant state changes
- **Debug**: URL extraction results, API responses
- **Warn**: unexpected but recoverable conditions
- **Error**: failures that prevent extraction

Guard `init()` logging with `if global.Log != nil` since `init()` runs before `TestMain` during tests.

### Registry Entry Fields

| Field | Type | Description |
|-------|------|-------------|
| `Factory` | `func(rid string, proxy *url.URL) (Extractor, error)` | Creates the extractor instance. Receives the room ID and optional proxy URL. |
| `Mobile` | `bool` | Whether to use mobile User-Agent for HTTP transport. Set `true` if the platform requires mobile headers. |
| `InitialError` | `int` | HTTP status code returned to the client when initial extraction fails. Use `500` for most platforms, `400` if bad room IDs cause the error. |

### File Organization

Split the package into multiple files by responsibility:

| File | Contents |
|------|----------|
| `<platform>.go` | Struct definition, `init()`, constructor, interface methods |
| `extract.go` | Stream URL resolution logic (`GetLink`, API calls) |
| `auth.go` | Authentication/token logic (if needed) |
| `room.go` | Room info resolution (if needed) |
| `types.go` | API response structs (if needed) |
| `<platform>_test.go` | Unit tests |

### Format Routing

The controller automatically dispatches to the correct forwarder based on the URL scheme and path extension returned by `Extract()`:

| URL Pattern | Forwarder | Content-Type |
|-------------|-----------|-------------|
| `http(s)://.../*.flv` or `*.xs` | FLV with header caching | `video/x-flv` |
| `http(s)://.../*.m3u8` | HLS (continuous TS pipe) | `video/mp2t` |
| `ws(s)://...` | WebSocket | — |

Your `SupportedFormats()` should return format identifiers that match the extensions the upstream URLs will have. For example, if the platform returns `.flv` URLs, include `"flv"` in the supported formats.

### HTTP Client

Use `httpweb.NewAddHeaderTransport` to create an HTTP transport that injects the appropriate User-Agent:

```go
// Desktop User-Agent
l.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, false)}

// Mobile User-Agent
l.client = &http.Client{Transport: httpweb.NewAddHeaderTransport(nil, true)}

// With proxy
l.client = &http.Client{
    Transport: httpweb.NewAddHeaderTransport(&http.Transport{Proxy: http.ProxyURL(proxy)}, false),
}
```

### Unit Tests

Write tests in `<platform>_test.go`. Key areas to test:

- `SupportedFormats()` returns the expected list
- `DefaultFormat()` returns the expected format
- Registry entry exists with correct `Mobile` and `InitialError` values
- Core extraction logic (mock HTTP servers with `httptest.NewServer`)

Initialize `global.Log` in `TestMain`:

```go
func TestMain(m *testing.M) {
    global.Log = logrus.New()
    global.Log.SetLevel(logrus.DebugLevel)
    os.Exit(m.Run())
}
```

## Complete Example: Minimal Extractor

Here is the smallest working extractor that returns a hardcoded URL:

```go
package Example

import (
    "net/url"

    "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor"
    "github.com/nv4d1k/live-stream-forwarder/global"
)

func init() {
    if global.Log != nil {
        log := global.Log.WithField("func", "app.engine.extractor.Example.init")
        log.Infoln("registering extractor")
    }
    extractor.Register("example", extractor.RegistryEntry{
        Factory: func(rid string, proxy *url.URL) (extractor.Extractor, error) {
            return &Link{rid: rid}, nil
        },
        Mobile:       false,
        InitialError: 500,
    })
}

type Link struct {
    rid string
}

func (l *Link) Extract(format string) (*extractor.Result, error) {
    return &extractor.Result{
        URL:     "https://cdn.example.com/live/" + l.rid + ".flv",
        Headers: nil,
    }, nil
}

func (l *Link) SupportedFormats() []string {
    return []string{"flv"}
}

func (l *Link) DefaultFormat() string {
    return "flv"
}
```

After adding `_ "github.com/nv4d1k/live-stream-forwarder/app/engine/extractor/Example"` to the controller's imports, the stream is immediately accessible at `http://localhost:8080/example/<room>`.
