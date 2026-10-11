# Live Stream Forwarder

Live Stream Forwarder (`lsf`) converts live streams from various platforms into locally accessible streams for video players (VLC, PotPlayer, etc.). It acts as a transparent proxy — no re-encoding, no transcoding.

## Supported Platforms

| Platform | URL Token | Example |
|----------|-----------|---------|
| [DouYu](https://www.douyu.com) | `douyu` | `http://localhost:8080/douyu/12345` |
| [HuYa](https://www.huya.com) | `huya` | `http://localhost:8080/huya/12345` |
| [BiliBili](https://live.bilibili.com) | `bilibili` | `http://localhost:8080/bilibili/12345` |
| [DouYin](https://live.douyin.com) | `douyin` | `http://localhost:8080/douyin/12345` |
| [Twitch](https://www.twitch.tv) | `twitch` | `http://localhost:8080/twitch/eslcs` |
| [Kick](https://kick.com) | `kick` | `http://localhost:8080/kick/eslcsb` |
| [YouTube](https://www.youtube.com) | `youtube` | `http://localhost:8080/youtube/WWTcu33u00A` |

## Install

Download the latest release from the [releases page](https://github.com/nv4d1k/live-stream-forwarder/releases).

### Docker

```bash
docker pull nv4d1k/live-stream-forwarder:latest
```

Or build from source:

```bash
docker build --build-arg VERSION=x.x.x --build-arg BUILD_TIME="$(date)" --build-arg SHA="$(git rev-parse HEAD)" -t lsf .
```

### Build from source

```bash
go build -o lsf .
```

## Usage

### Start the service

```bash
# Default: listen on 127.0.0.1 with a random port
lsf

# Specify address and port
lsf -l 0.0.0.0 -p 8080

# With HTTP proxy
lsf --proxy http://user:pass@host:port

# Debug mode (enables /debug/* endpoints including pprof)
lsf --log-level 6

# Log to file
lsf --log-file lsf.log
```

### Open stream in player

```
http://<address>:<port>/<platform>/<room_id>
```

For example, to watch DouYu room 12345:

```
http://127.0.0.1:8080/douyu/12345
```

### Per-request proxy

```
http://<address>:<port>/<platform>/<room_id>?proxy=http://user:pass@host:port
```

### Format selection

Some platforms support multiple stream formats. Use the `?format=` query parameter:

```
http://<address>:<port>/huya/12345?format=flv
http://<address>:<port>/huya/12345?format=hls
```

Available formats by platform:

| Platform | Formats | Default |
|----------|---------|---------|
| DouYu | flv, m3u8, ws | flv |
| HuYa | flv, hls | flv |
| BiliBili | flv, m3u8 | flv |
| DouYin | flv, m3u8 | flv |
| Twitch | m3u8 | m3u8 |
| Kick | m3u8 | m3u8 |
| YouTube | m3u8, dash, sabr | sabr |

YouTube defaults to the `sabr` format — it speaks the server-ABR (UMP) protocol upstream and forwards a two-track Matroska stream (VP9 video + AAC audio interleaved in one stream) at **every resolution the room provides, 1440p60 and 2160p60 included**:

```
http://<address>:<port>/youtube/WWTcu33u00A
```

Pass `?format=dash` for the 1080p60 fMP4 (H.264) path, or `?format=m3u8` for the HLS path (muxed TS variants up to 1080p):

```
http://<address>:<port>/youtube/WWTcu33u00A?format=dash
http://<address>:<port>/youtube/WWTcu33u00A?format=m3u8
```

All clients of the same room (and quality) share one upstream session: the first viewer starts it, later viewers attach instantly with a replay of the buffered stream.

### Quality selection

YouTube streams can be capped at a specific resolution with the `?quality=` query parameter (e.g. `480p` or `480`). The tallest variant not exceeding the requested height is picked:

```
http://<address>:<port>/youtube/WWTcu33u00A?quality=720p
```

Without the parameter the highest available resolution is served — on the default `sabr` format this can be 1440p60 or 2160p60. On `sabr` the quality also keys the shared upstream, so two clients asking different qualities never share (or duplicate) an upstream session.

> **Note**: YouTube URLs are bound to the proxy exit IP. Extraction and streaming must go through the same proxy — configure the global `--proxy` flag or pass `?proxy=` on the request (mandatory from mainland China).

## Features

- **Seamless 403 recovery**: When an upstream stream URL expires (HTTP 403), the forwarder automatically re-extracts a fresh URL and reconnects — the player never sees a break.
- **Transport fallback**: DouYu decides per request whether a room is served over p2p (wss) or its http CDN, and flips between them mid-stream. Both carry the same FLV payload, so the WebSocket forwarder transparently rides the http upstream while p2p is unavailable and returns to wss when it recovers — the player never sees a break or a format change.
- **Stall watchdogs**: Upstream nodes sometimes stop pushing data while keeping the connection open (e.g. DouYu's xp2p edges after token expiry). Two watchdogs catch this — one for silent connections, one for streams whose data never reaches the client — and reconnect automatically. Long sessions also self-heal expired platform credentials.
- **Proactive token refresh**: For platforms with expiring URLs (e.g. Kick's JWT-signed playback URL, DouYu's 5-minute ws tokens), the HLS and WebSocket forwarders proactively re-extract before the token expires, avoiding playback interruptions entirely.
- **Best quality by default**: HLS streams automatically select the highest bandwidth variant; YouTube's default SABR channel serves every resolution the room provides (1440p60/2160p60 included) as a remuxed MKV.
- **Shared upstream fan-out**: All clients watching the same YouTube room share one upstream session — one extraction, one upstream connection, one proxy's worth of bandwidth, regardless of viewer count; late joiners replay the buffered stream instantly.
- **FLV header caching**: Late-joining clients receive a cached FLV header before live data, enabling mid-stream connections without player errors.
- **No re-encoding**: Streams are forwarded as-is, keeping latency minimal.

## Development

- [Writing a Custom Extractor](docs/writing-extractor.md)

## License

[MIT](LICENSE.txt)
