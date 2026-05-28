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

# Format
gofmt -w .

# Docker build (multi-arch image: nv4d1k/live-stream-forwarder)
docker build --build-arg VERSION=x.x.x --build-arg BUILD_TIME="$(date)" --build-arg SHA="$(git rev-parse HEAD)" .
```

### CLI flags and env vars

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `-l, --listen-address` | `LISTEN_ADDR` | `127.0.0.1` | Bind address |
| `-p, --listen-port` | `LISTEN_PORT` | `0` (random) | Bind port |
| `--proxy` | — | — | Global HTTP proxy URL |
| `--log-level` | — | `3` (warn) | 0–6; 6 = debug, enables `/debug/*` endpoints |
| `--log-file` | — | stdout | Log file path (also outputs to stdout) |

## Architecture

Live Stream Forwarder (`lsf`) converts live streams from Chinese and international platforms into locally accessible streams for video players (VLC, PotPlayer). It acts as a proxy — no re-encoding.

### Two-layer engine under `app/engine/`

**Extractors** (`app/engine/extractor/<Platform>/`) — resolve a room ID to a stream URL:

All extractors implement the unified `Extractor` interface (`app/engine/extractor/extractor.go`):
- `Extract(format) (*Result, error)` — returns URL + required headers + optional ExpireAt
- `SupportedFormats() []string` — e.g. `["flv", "m3u8"]`
- `DefaultFormat() string` — fallback when caller doesn't specify

Each platform registers itself via `init()` calling `extractor.Register(name, RegistryEntry)` with a `Factory`, `Mobile` flag, and `InitialError` HTTP status code. The controller looks up platforms by name from `extractor.Registry` — no per-platform switch-case.

Platform specifics:
- **DouYu**: MD5 auth chain, encryption data, supports p2p (ws/wss) via `p2p` field (0/2/9/10); `SupportedFormats: ["flv", "m3u8", "ws"]`
- **HuYa**: goja JS VM to parse `HNF_GLOBAL_INIT` from page HTML, anti-code processing; `GetLink(format)` accepts `flv`/`hls`; `m3u8` is normalized to `hls` internally
- **BiliBili**: v1 API (`/room/v1/Room/playUrl`) tried first for higher quality (returns qn=10000 for unauthenticated users), falls back to v2 API (`getRoomPlayInfo`) which may degrade quality; `Mobile: false`; Referer header required
- **DouYin**: cookie auth (`__ac_nonce`/`ttwid`), extracts JSON from `self.__pace_f.push` in `<script>` tags
- **Twitch**: hardcoded public Client-ID (`kimne78kx3ncx6brgo4mv6wki5h1ko`), GraphQL `PlaybackAccessToken_Template` for sig+token, Usher HLS master playlist; `SupportedFormats: ["m3u8"]`
- **Kick**: public `/api/v2/channels/<slug>` returns `playback_url` (AWS IVS HLS master + ES384 JWT). No auth/cookies needed. JWT `exp` claim parsed via `parseJWTExp()` and set as `Result.ExpireAt`. Headers include `Referer` and `Origin` for AWS IVS origin enforcement. `Extract()` calls API every time (not cached in constructor) so 403 retries auto-refresh the JWT. `SupportedFormats: ["m3u8"]`

**Forwarders** (`app/engine/forwarder/<type>/`) — pipe stream data from upstream to client:

All forwarders use a **Pipe-based architecture** for seamless 403 recovery. When an upstream URL expires (403), the producer goroutine re-calls the extractor to get a fresh URL and reconnects — the client never sees a break.

- `stream/` — shared `Pipe` (goroutine-safe buffered io.Reader/io.Writer) and `Stream` (producer goroutine with infinite 403 retry loop). `ExtractFunc` signature is `func(previous *ExtractResult) (*ExtractResult, error)` — receives the previous result on retries so format consistency (scheme + path extension) can be validated via `formatMatches()`. Retries are unlimited; only stops on client disconnect or non-retriable errors. `ExtractResult` carries `ExpireAt *time.Time` so forwarders can proactively refresh before URL expiry.
- `httpweb/` — `Stream(extractFn)` returns `*stream.Stream` for HTTP/FLV; `Forward()` kept for backward compat. `AddHeaderTransport` injects User-Agent (desktop or mobile).
- `flv/` — `FLVStream` wraps a `*stream.Stream` and prepends cached FLV header for mid-stream client connections. `HeaderCache` is keyed by `"platform:room"`, process-wide via `DefaultCache`. `HeaderCacheWriter` captures the first bytes of the stream as the FLV header.
- `hls/` — `HLSStream` has its own produce loop: fetches/parse m3u8, downloads segments, pipes raw MPEG-TS. Master playlist variant selection picks **highest BANDWIDTH** (`pickHighestBandwidthVariant`). 403 on any playlist/segment clears `mediaPlaylistURL` and re-extracts. **Proactive token refresh**: when `ExtractResult.ExpireAt` is set, a `time.AfterFunc` timer fires 60s before expiry (minimum 5s from now) and sends on `refreshCh`, causing the produce loop to re-extract before the URL expires. Platforms without `ExpireAt` are unaffected.
- `websocket/` — `WebSocketForwarderWithRetry(extractFn)` uses `XP2PClientWithRetry` which validates re-extracted URLs are still ws/wss and reconnects within `ReadLoop`.

### HTTP layer

- `cmd/root.go` — Cobra CLI, sets up Gin router with CORS and proxy middleware. Single route: `GET /:platform/:room`
- `controllers/forwarder.go` — registry-based dispatch: looks up platform in `extractor.Registry`, creates extractor, builds `ExtractFunc` closure with format consistency checks, dispatches to forwarder by URL scheme/extension via `dispatchStream()`. Format resolution: `?format=` query param → extractor's `SupportedFormats()` → random pick between flv/m3u8 if both available → `DefaultFormat()`. Per-request `?proxy=` overrides the global `--proxy` flag.
- `controllers/debug.go` — pprof and debug endpoints, only registered when `--log-level 6`

### Request flow

```
GET /douyu/12345
  → controller: extractor.Registry["douyu"].Factory("12345", proxy)
  → ext.Extract(desiredFormat) → *Result{URL, Headers, ExpireAt}
  → extractFn closure wraps ext.Extract with format consistency checks
  → extractFn(nil) → first extraction → determine initialFormat
  → dispatchStream by scheme/extension:
      http(s) + .flv  → flvStreamWithCache → streamToClient (video/x-flv)
      http(s) + .m3u8 → hls.Stream → streamToClient (video/mp2t)
      ws(s)           → websocket.ForwarderWithRetry

  Producer goroutine (inside Stream or HLSStream):
    extractFn(previous) → fetch → io.Copy into Pipe
    on 403 → extractFn(previous) again → reconnect → continue
    on client disconnect → Pipe.BreakWithError → stop
```

### Global state (`global/`)

- `Version`, `BuildTime`, `GitCommit` — set via ldflags at build time
- `Log` (logrus), `LogLevel` (0–6; 6 = debug, enables `/debug/*` endpoints)
- Desktop and mobile User-Agent constants

## Key patterns

- **Extractor registry**: Platforms self-register via `init()`. Adding a new platform requires: (1) implement `Extractor` interface, (2) call `extractor.Register()` in `init()`, (3) add blank import in `controllers/forwarder.go`. No controller changes needed.
- **HLS variant selection**: `pickHighestBandwidthVariant` in `hls/hlsstream.go` always selects the highest bandwidth variant from master playlists, giving the best quality across all HLS platforms.
- **FLV header caching**: `flv.DefaultCache` stores the FLV header per `"platform:room"` key so late-joining clients receive the header before live data, enabling mid-stream connection without player errors.
- **Format consistency on retry**: `ExtractFunc(previous)` receives the previous result on retry. The controller and `Stream.produce()` both validate that re-extracted URLs have the same scheme and path extension (e.g. won't switch from FLV to m3u8 mid-stream).
- **Proactive token refresh (HLS)**: `ExtractResult.ExpireAt` signals when a URL will expire. The HLS forwarder schedules a `time.AfterFunc` to re-extract 60s before expiry (adjusting for short-lived tokens). The FLV/WebSocket paths rely on 403-triggered re-extraction only. Extractors that don't set `ExpireAt` are unaffected.
- **Proxy threading**: CLI `--proxy` flag or per-request `?proxy=` query param → Gin middleware sets in context → passed through extractors and forwarders.
- **Gin version is pinned to 1.11.0** (downgraded due to nil pointer issue in Docker containers).
- **403 retry detection**: `isRetriable()` in `stream/stream.go` checks for "403" substring in error messages — this is string-based, not status-code-based.
- **`.xs` extension**: `dispatchStream` treats `.xs` the same as `.flv` (both route to FLV forwarder).
- **HuYa format normalization**: HuYa normalizes `m3u8` to `hls` internally; the controller uses the raw format string from the extractor.
- **CI/CD**: Docker builds produce multi-arch images (linux/386, amd64, arm/v6, arm/v7, arm64, ppc64le, riscv64, s390x). Release workflow builds for linux/windows/darwin × amd64/arm64.
