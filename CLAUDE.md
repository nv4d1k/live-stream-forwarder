# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Git Conventions

- **All commits must be GPG-signed.** Never use `--no-gpg-sign`. If signing fails in a non-interactive environment (pinentry cannot prompt for the passphrase), stop and hand the commit command to the user to run in an interactive terminal — do not fall back to an unsigned commit. Note the `!` prefix in a Claude session has no TTY either; the durable fix on this Mac is `brew install pinentry-mac` plus `pinentry-program <path>/pinentry-mac` in `~/.gnupg/gpg-agent.conf` (GUI prompt works from any environment).
- Commit messages in English, format: `type(scope): description`.

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

# Run with BiliBili cookie for authenticated streams
go run . --bilibili-cookie "SESSDATA=xxx; bili_jct=xxx; DedeUserID=xxx"

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
| `--bilibili-cookie` | — | — | Raw cookie string for BiliBili authenticated streams |
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

Each platform registers itself via `init()` calling `extractor.Register(name, RegistryEntry)` with a `Factory`, a `UserAgent` (injected into the forwarders' transports; empty = default desktop UA, e.g. the mobile UA for HuYa), and `InitialError` HTTP status code. The controller looks up platforms by name from `extractor.Registry` — no per-platform switch-case.

Platform specifics:
- **DouYu**: MD5 auth chain, encryption data, supports p2p (ws/wss) via `p2p` field (0/2/9/10); `SupportedFormats: ["flv", "m3u8", "ws"]`. URL expiry is parsed from the `expire`/`txTime` query params into `Result.ExpireAt` (earliest wins, `expireAtFromURL`). Credentials are self-healing: the auth timestamp (`t10`) is refreshed on every rate-stream request, and a rejected request — including the plain-text `鉴权失败` the API answers when `enc_data` expires (~10 min) — triggers an `enc_data` renewal plus one retry (`getRateStream`/`refreshEncData`). API-level errors surface as errors instead of URLs built from empty fields (`rateStreamError`).
- **HuYa**: goja JS VM to parse `HNF_GLOBAL_INIT` from page HTML, anti-code processing; `GetLink(format)` accepts `flv`/`hls`; `m3u8` is normalized to `hls` internally
- **BiliBili**: v2 API only (`getRoomPlayInfo` with `qn=10000`); requires Cookie authentication via `?cookie=` query param for qn=10000 (原画); without cookie returns qn=250 (720p); implements `extractor.CookieSetter` for cookie injection into API requests via `cookieTransport`; Referer header required
- **DouYin**: room page hit issues `ttwid` cookie (one retry carrying `__ac_nonce` when rate-limited), then `GET /webcast/room/web/enter/` API (the four `browser_*` params are mandatory, `msToken`/`room_id_str` are not) returns `stream_url.flv_pull_url`/`hls_pull_url_map` keyed by quality; picker takes the highest of `FULL_HD1>HD1>SD1>SD2` (`pickStreamURL`). URL `expire` param is an absolute Unix ts parsed into `Result.ExpireAt` (`expireAtFromURL`). Stream URLs need no Referer; FLV URLs 302-redirect to a bare IP (Go client follows automatically)
- **Twitch**: hardcoded public Client-ID (`kimne78kx3ncx6brgo4mv6wki5h1ko`), GraphQL `PlaybackAccessToken_Template` for sig+token, Usher HLS master playlist; `SupportedFormats: ["m3u8"]`
- **Kick**: public `/api/v2/channels/<slug>` returns `playback_url` (AWS IVS HLS master + ES384 JWT). No auth/cookies needed. JWT `exp` claim parsed via `parseJWTExp()` and set as `Result.ExpireAt`. Headers include `Referer` and `Origin` for AWS IVS origin enforcement. `Extract()` calls API every time (not cached in constructor) so 403 retries auto-refresh the JWT. `SupportedFormats: ["m3u8"]`
- **YouTube**: room is the 11-char video ID (validated `^[\w-]{11}$`). Anonymous ANDROID Innertube client (`POST /youtubei/v1/player`, no cookies, no PO tokens — client constants centralized in `innertube.go` for policy-driven updates) returns `streamingData.hlsManifestUrl` (muxed TS variants 144p–1080p) and `streamingData.dashManifestUrl` (fMP4 DASH representations); `Extract(format)` picks by format (`"m3u8"` for HLS, `"dash"` — the default via `DefaultFormat()` — for DASH). Manifest URLs carry `/expire/<unix>/` (~6h) parsed into `Result.ExpireAt` via `expireAtFromManifestURL`; the URLs are signature-locked (`sparams`+`sig` — never rewrite params) and IP-bound to the proxy exit, so extraction and streaming must share one proxy (mandatory from mainland China). The DASH manifest URL ends in a signature, not `.mpd` — the controller matches `/api/manifest/dash/` instead (`isDASHManifestURL`). Implements `extractor.QualitySetter`: `?quality=` (e.g. `720p`) builds a `VariantSelector` for the HLS path that picks the tallest variant not exceeding the requested height (all-taller → shortest; unparsable resolutions → highest bandwidth); `dash` always picks the highest-bandwidth representations. The innertube request needs the ANDROID User-Agent, so the extractor uses a bare `http.Transport` (request-level UA) instead of `httpweb.AddHeaderTransport` (which would append a second desktop UA). `SupportedFormats: ["m3u8", "dash"]`

**Forwarders** (`app/engine/forwarder/<type>/`) — pipe stream data from upstream to client:

All forwarders use a **Pipe-based architecture** for seamless 403 recovery. When an upstream URL expires (403), the producer goroutine re-calls the extractor to get a fresh URL and reconnects — the client never sees a break.

- `stream/` — shared `Pipe` (goroutine-safe buffered io.Reader/io.Writer) and `Stream` (producer goroutine with infinite 403 retry loop). `ExtractFunc` signature is `func(previous *ExtractResult) (*ExtractResult, error)` — receives the previous result on retries so format consistency (scheme + path extension) can be validated via `formatMatches()`. Retries are unlimited; only stops on client disconnect or non-retriable errors. `ExtractResult` carries `ExpireAt *time.Time` so forwarders can proactively refresh before URL expiry.
- `httpweb/` — `Stream(extractFn)` returns `*stream.Stream` for HTTP/FLV; `Forward()` kept for backward compat. `AddHeaderTransport` injects a configurable User-Agent (`NewAddHeaderTransport(T, userAgent)`, empty = default desktop UA) — the controller passes the platform's `RegistryEntry.UserAgent` through every forwarder constructor, so future extractors can declare arbitrary UAs without new flags.
- `flv/` — `FLVStream` wraps a `*stream.Stream` and prepends the cached FLV header before the media data on first Read (prepend when ready; wait up to `flv.HeaderWaitTimeout` when cold; headerless passthrough on timeout or missing entry). `HeaderCache` is keyed by `"platform:room"`, process-wide via `DefaultCache`. `HeaderCacheWriter` captures the FLV header and **strips it from the pipe** — the pipe carries media data only, and every upstream reconnect's fresh header is stripped again, so a client stream never contains a second `FLV` signature (doubled headers desync demuxers: PotPlayer refuses to play, ffprobe reports `Packet mismatch`).
- `hls/` — `HLSStream` has its own produce loop: fetches/parse m3u8, downloads segments, pipes raw MPEG-TS. Master playlist variant selection picks **highest BANDWIDTH** (`pickHighestBandwidthVariant`). 403 on any playlist/segment clears `mediaPlaylistURL` and re-extracts. **Proactive token refresh**: when `ExtractResult.ExpireAt` is set, a `time.AfterFunc` timer fires 60s before expiry (minimum 5s from now) and sends on `refreshCh`, causing the produce loop to re-extract before the URL expires. Platforms without `ExpireAt` are unaffected.
- `dash/` — `DASHStream` forwards a YouTube live DASH MPD as an interleaved two-track fMP4 stream (served as `video/mp4`). It parses the MPD (`mpd.go`: audio/video AdaptationSets, per-Representation signed `BaseURL`, `sq/<n>` SegmentURLs), picks the highest-bandwidth representation of each set, and fetches one segment batch per track per cycle — **both tracks in parallel** (a `BaseURL + sq/<n>` GET returns exactly that segment: init + moofs). **Segment URLs carry no `lmt` suffix**: the MPD's per-segment `lmt` values are version markers, and requesting a stale one is rejected with 404 by part of the CDN fleet (a hardcoded `lmt/1` worked on some rooms and hung others in an infinite init-probe retry — the "PotPlayer loads forever on some rooms" bug); without the suffix the server serves the current version. **Session window**: YouTube serves each innertube-signed DASH session for only ~30 seconds (`playlist_duration/30`) and then answers 403 for every request; `sessionRenewInterval` (24s) proactively re-runs innertube+MPD and swaps fresh BaseURLs (`renew`, keeping the sq/tfdt cursors) so the stream never gaps — a passive 403 re-extract remains as fallback, and a failing renewal backs off exponentially (`renewBackoffBase`→`renewBackoffMax`) instead of hot-looping the innertube API. There is no fixed poll sleep: requests ahead of the live edge are held by the server until the segment is generated, pacing the loop at segment rate (~1.9s/cycle measured). **Fast startup**: the first round probes both inits **in parallel** with `fetchInitOnly` — each response is closed at its first moof so the media payload is never downloaded — then merges and pipes the combined init; probe failures (non-403) count toward `notFoundLimit` and re-extract once exhausted, so a probe that can never succeed cannot spin forever leaving the client on an empty response. Media batches stream box-by-box through `boxReader`. A moof and its following boxes (mdat, emsg) are piped as one atomic group — with parallel tracks, a moof separated from its mdat desyncs stream demuxers (ffmpeg: "Invalid NAL unit size"). A track's sq advances only after a batch completes cleanly, so an interrupted batch re-fetches the same sq and dedupes by `tfdt` (`tfdtCursor` survives renewal and re-extraction so the client stream never rewinds). **Two-track merging** (`box.go`): both tracks' moovs declare `track_ID` 1, so the inits are merged into one moov (`mergeInits`: video trak's tkhd and the video mvex/trex are renumbered to track_ID 2, audio mvhd's `next_track_ID` to 3, single ftyp) and every video moof's `traf/tfhd.track_ID` is rewritten 1→2 — plain 4-byte field writes, no re-encoding. The trex merge matters: without a trex per track, strict demuxers (ffmpeg-based players like PotPlayer) reject the fragments ("could not find corresponding trex") even though VLC tolerates it. Batches strip their repeated init (everything before the first moof is skipped). Starts `startBackoff` (3) segments behind the live edge, clamped to the MPD window start. Persistent failures retry the same sq up to `notFoundLimit` times; `ExpireAt` schedules proactive refresh like the HLS forwarder. Verified end-to-end: VLC playing the forwarded live stream, ffmpeg pipe-mode (non-seekable input, PotPlayer's demux scenario) decoding both tracks at full 60fps, and a 60s endurance run with zero 403s and two seamless renewals.
- `dash/` **fan-out hub** (`hub.go`) — one upstream `DASHStream` (the "core") per `platform:room|proxy` key, shared by any number of clients. `Hub.GetOrCreate` creates the core once; a pump goroutine reads the core's stream box-by-box, captures the merged init and republishes each completed group (a moof+mdat pair — the mdat completes it, so a fast audio group is never held back waiting for a slow high-bitrate video batch) into a ring buffer (last 16 groups / 32MB). `Subscribe()` replays the buffered init + groups to the joiner under the lock, then tails live data; the HTTP layer's fast path (`Hub.SubscribeExisting`) serves a joining client with **no extraction at all** (TTFB in milliseconds vs ~1s innertube round trip) when a stream is already live. Subscribers that stop reading are cut at 48MB backlog (`Hub.KickBytes`, `errSlowSubscriber` — they reconnect and rejoin from the live edge); when the last subscriber leaves, the core is torn down after `Hub.IdleGrace` (30s, absorbing player probe reconnects), stopping all upstream traffic. Core death (e.g. `ErrFormatDeadlock`) closes every subscriber with the error and removes the hub entry. N clients cost 1 innertube session, 1 pair of segment fetches per cycle and one proxy's worth of bandwidth.
- `websocket/` — `WebSocketForwarderWithRetry(extractFn)` uses `XP2PClientWithRetry` which validates re-extracted URLs are still ws/wss and reconnects within `ReadLoop`. `ReadLoop` arms `SetReadDeadline` with the earliest of three caps: a 30s message watchdog (an xp2p edge node that stops pushing data after token expiry but keeps the connection open would otherwise block `ReadMessage` forever), a 60s pipe-write watchdog (`markingWriter` records when data actually reaches the pipe, catching "fake alive" streams whose frames keep arriving but are buffered indefinitely by the header cache writer), and the proactive refresh point (`ExpireAt` − 60s). Timeout/403 errors go through `reconnect()` with backoff (3s→5s→10s, max 5 consecutive failures, client disconnect stops immediately); each successful reconnect resets the pipe-stall timer so the fresh connection gets a full window.

### HTTP layer

- `cmd/root.go` — Cobra CLI, sets up Gin router with CORS and proxy middleware. Routes: `GET /tools/cookie`, `GET /:platform/:room`
- `controllers/forwarder.go` — registry-based dispatch: looks up platform in `extractor.Registry`, creates extractor, builds `ExtractFunc` closure with format consistency checks, dispatches to forwarder by URL scheme/extension via `dispatchStream()`. Format resolution: `?format=` query param → extractor's `SupportedFormats()` → random pick between flv/m3u8 if both available → `DefaultFormat()`. Per-request `?proxy=` overrides the global `--proxy` flag. Per-request `?cookie=` (gzip+base64url encoded) overrides the global `--bilibili-cookie` flag; decoded and injected via `extractor.CookieSetter` if the extractor implements it. Per-request `?quality=` is injected via `extractor.QualitySetter` (`applyQualityHint`) when the extractor implements it. DASH requests short-circuit through `dash.DefaultHub.SubscribeExisting(platform:room|proxy)` when a shared stream is already live — no extraction, instant buffered replay. `streamToClient` bridges the forwarder reader through a goroutine and `select`s on the request context, so a client that walks away during an upstream stall (DASH video batches can crawl for many seconds) is noticed immediately and the reader is closed — without this, a blocked Read could never observe the write failing, the dash hub subscriber count would leak and the shared core would keep fetching upstream data forever after the last client left.
- `controllers/debug.go` — pprof and debug endpoints, only registered when `--log-level 6`
- `controllers/tools.go` — `CookieTool` handler at `GET /tools/cookie`, serves embedded HTML page (`static/cookie.html`) for encoding cookies into gzip+base64url query-safe strings (works for any platform that supports `?cookie=`)
- `controllers/cookie.go` — `decodeCookie` utility: base64url decode → gzip decompress → raw cookie string

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
      http(s) + dash manifest (path /api/manifest/dash/ or .mpd)
                      → dash.DefaultHub fan-out → streamToClient (video/mp4)
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
- **FLV header caching**: `flv.DefaultCache` stores the FLV header per `"platform:room"` key. `HeaderCacheWriter` strips the header (and each reconnect's fresh header) from the pipe; `FLVStream`/`WebSocketForwarder` prepend the cached copy, so every client stream starts with exactly one header and late-joining clients get a valid stream immediately. Non-FLV upstreams resolve the entry via `SetMissing()` so consumers never block forever.
- **Format consistency on retry**: `ExtractFunc(previous)` receives the previous result on retry. The controller and `Stream.produce()` both validate that re-extracted URLs have the same scheme and path-extension family (`.xs` counts as `.flv` — DouYu p2p=2 serves identical FLV payload under it, so that switch reconnects seamlessly). When the platform permanently switches a room's protocol mid-stream (e.g. DouYu moving a room to p2p/ws), the controller tolerates 3 consecutive mismatches, then reports `stream.ErrFormatDeadlock`; all three producers (FLV `Stream.produce`, HLS `HLSStream.produce`, WebSocket `client.reconnect`) treat it as terminal — they close the client stream so its reconnect re-dispatches into the forwarder matching the new format. There is no way to hand a live HTTP-FLV response to the WebSocket forwarder mid-stream.
- **Proactive token refresh (FLV, HLS & WebSocket)**: `ExtractResult.ExpireAt` signals when a URL will expire. The FLV path (`stream.Stream.produce`) closes the upstream body ~60s before expiry (via `time.AfterFunc` + `body.Close()`, minimum 5s for short-lived tokens) and reconnects cleanly; the HLS forwarder schedules a `time.AfterFunc` to re-extract 60s before expiry; the WebSocket forwarder caps its read deadline at `ExpireAt` − 60s so the swap happens while data is still flowing. BiliBili parses its absolute `expires` param, DouYu its relative `expire`/hex `txTime`. Extractors that don't set `ExpireAt` fall back to error-driven refresh.
- **Stall watchdogs (WebSocket)**: two-layer liveness detection — message-level (30s without any ws frame) and pipe-level (60s without data reaching the pipe, tracked via `markingWriter`). The pipe layer catches streams whose frames keep arriving but never produce consumable data (indefinite header-detection buffering), which message-level detection alone cannot see.
- **Proxy threading**: CLI `--proxy` flag or per-request `?proxy=` query param → Gin middleware sets in context → passed through extractors and forwarders.
- **Optional CookieSetter interface**: `extractor.CookieSetter` is an optional interface that extractors can implement to receive a raw cookie string. The controller type-asserts after factory creation and calls `SetCookie(rawCookie)` if the `?cookie=` query param is present. Only BiliBili implements it currently. Other extractors are unaffected.
- **Optional QualitySetter interface**: `extractor.QualitySetter` is an optional interface that extractors can implement to receive a quality hint (e.g. `720p`) via the `?quality=` query param, used to build a `Result.VariantSelector` for the HLS forwarder. YouTube implements it; extractors without variant selection are unaffected.
- **Cookie encoding**: `?cookie=` value is gzip-compressed + base64url-encoded (no padding) to avoid URL issues with semicolons and special characters in raw cookies. Decoded server-side in `controllers/cookie.go`. A tool page at `/tools/cookie` provides client-side encoding using the browser CompressionStream API.
- **Gin version is pinned to 1.11.0** (downgraded due to nil pointer issue in Docker containers).
- **Retriable error detection**: `isRetriable()` in `stream/stream.go` treats URL staleness (403/404/410), upstream faults (5xx) and transient network conditions (reset/refused/EOF/timeout) as retriable — re-extraction heals all of them; deterministic errors (e.g. 400) stay fatal. Every produce-loop failure sleeps an exponential backoff (`retryBackoffBase`→`retryBackoffMax`, 1s→10s) before retrying, so API hiccups can't hot-loop against the platform (DouYu -412 rate-limiting). Both bounds are atomics so tests can retune them while earlier tests' producer goroutines wind down without tripping `-race`.
- **DASH fan-out**: all clients of one `platform:room|proxy` share a single upstream core via `dash.DefaultHub` (see the `dash/` forwarder entry); joining clients replay the buffered init + recent groups instantly (`Hub.SubscribeExisting` skips extraction), idle cores are torn down after 30s. FLV/HLS remain per-client (their upstreams are cheap CDN URLs with no per-session signing window).
- **`.xs` extension**: `dispatchStream` treats `.xs` the same as `.flv` (both route to FLV forwarder), and the format-consistency checks treat them as the same family.
- **HuYa format normalization**: HuYa normalizes `m3u8` to `hls` internally; the controller uses the raw format string from the extractor.
- **CI/CD**: Docker builds produce multi-arch images (linux/386, amd64, arm/v6, arm/v7, arm64, ppc64le, riscv64, s390x). Release workflow builds for linux/windows/darwin × amd64/arm64.
