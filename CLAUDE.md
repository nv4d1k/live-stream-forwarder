# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build
go build -o lsf .

# Build with version info (as done in CI/Dockerfile)
go build -trimpath -ldflags="-X 'github.com/nv4d1k/live-stream-forwarder/global.Version=<ver>' -X 'github.com/nv4d1k/live-stream-forwarder/global.BuildTime=<time>' -X github.com/nv4d1k/live-stream-forwarder/global.GitCommit=<sha>" -o lsf .

# Run (defaults to 127.0.0.1 with random port)
go run . -l <address> -p <port>

# Run with debug logging (enables /debug/* endpoints including pprof)
go run . --log-level 6

# Run with HTTP proxy
go run . --proxy http://user:pass@host:port

# Run tests
go test -cover -v ./...

# Run single package tests
go test -cover -v ./app/engine/extractor/DouYu/...

# Docker build
docker build --build-arg VERSION=x.x.x --build-arg BUILD_TIME="$(date)" --build-arg SHA="$(git rev-parse HEAD)" .
```

## Architecture

Live Stream Forwarder (`lsf`) converts live streams from Chinese and international platforms into locally accessible streams for video players (VLC, PotPlayer). It acts as a proxy — no re-encoding.

### Two-layer engine under `app/engine/`

**Extractors** (`app/engine/extractor/<Platform>/`) — resolve a room ID to a stream URL:
- Each has a `Link` struct with `New<Platform>Link(rid, proxy)` and `GetLink()` returning `*url.URL`
- DouYu: MD5 auth chain, encryption data, supports p2p (ws/wss) via `p2p` field (0/2/9/10)
- HuYa: goja JS VM to parse `HNF_GLOBAL_INIT` from page HTML, anti-code processing; `GetLink(format)` accepts `?format=flv|hls`
- BiliBili: two API versions (v1 fallback to v2), Referer header required
- DouYin: cookie auth (`__ac_nonce`/`ttwid`), extracts JSON from `self.__pace_f.push` in `<script>` tags
- Twitch: GraphQL token/sig exchange, client ID scraped from page

**Forwarders** (`app/engine/forwarder/<type>/`) — pipe stream data from upstream to client:

All forwarders use a **Pipe-based architecture** for seamless 403 recovery. When an upstream URL expires (403), the producer goroutine re-calls the extractor to get a fresh URL and reconnects — the client never sees a break.

- `stream/` — shared `Pipe` (goroutine-safe buffered io.Reader/io.Writer) and `Stream` (producer goroutine with infinite 403 retry loop). `ExtractFunc` signature is `func(previous *ExtractResult) (*ExtractResult, error)` — receives the previous result on retries so format consistency (scheme + path extension) can be validated via `formatMatches()`. Retries are unlimited; only stops on client disconnect or non-retriable errors.
- `httpweb/` — `Stream(extractFn)` returns `*stream.Stream` for HTTP/FLV; `Forward()` kept for backward compat. `AddHeaderTransport` injects User-Agent (desktop or mobile).
- `hls/` — `StreamSegment(extractFn)` for non-m3u8 segment data via pipe. Playlist methods (`ForwardM3u8`, `WrapPlaylist`) remain direct — playlists are short-lived, not piped. All segment/variant URLs are rewritten to self-referencing URLs with original URL base64-encoded in `?url=` param.
- `websocket/` — `WebSocketForwarderWithRetry(extractFn)` uses `XP2PClientWithRetry` which validates re-extracted URLs are still ws/wss and reconnects within `ReadLoop`.

### HTTP layer

- `cmd/root.go` — Cobra CLI, sets up Gin router with CORS and proxy middleware. Single route: `GET /:platform/:room`
- `controllers/forwarder.go` — dispatches by platform name, creates `ExtractFunc` closures with format consistency checks, selects forwarder by URL scheme/extension. Segment requests (`?url=` param) return 302 redirect to main room URL on 403, causing player to re-fetch playlist through the pipe-based path.

### Request flow

```
GET /douyu/12345
  → controller creates extractFn closure
  → extractFn(nil) → first extraction → determine scheme
  → scheme http/https → httpweb.Stream(extractFn) → streamToClient()
  → scheme ws/wss → websocket.ForwarderWithRetry(extractFn).Start()

  Producer goroutine inside Stream:
    extractFn(previous) → fetch → io.Copy into Pipe
    on 403 → extractFn(previous) again → reconnect → continue
    on client disconnect → Pipe.BreakWithError → stop
```

### Global state (`global/`)

- `Version`, `BuildTime`, `GitCommit` — set via ldflags at build time
- `Log` (logrus), `LogLevel` (0–6; 6 = debug, enables `/debug/*` endpoints)
- Desktop and mobile User-Agent constants

## Key patterns

- HLS URL rewriting: m3u8 playlists have all URLs rewritten to route back through the service itself (`prefix` URL), with the original URL base64-encoded in `?url=`. This allows proxying each segment individually.
- Proxy threading: CLI `--proxy` flag or per-request `?proxy=` query param → Gin middleware sets in context → passed through extractors and forwarders.
- Format consistency on retry: `ExtractFunc(previous)` receives the previous result on retry. The controller and `Stream.produce()` both validate that re-extracted URLs have the same scheme and path extension (e.g. won't switch from FLV to m3u8 mid-stream).
- Gin version is pinned to 1.11.0 (downgraded due to nil pointer issue in Docker containers).
