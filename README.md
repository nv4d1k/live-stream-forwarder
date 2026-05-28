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

## Features

- **Seamless 403 recovery**: When an upstream stream URL expires (HTTP 403), the forwarder automatically re-extracts a fresh URL and reconnects — the player never sees a break.
- **Proactive token refresh**: For platforms with expiring URLs (e.g. Kick's JWT-signed playback URL), the HLS forwarder proactively re-extracts before the token expires, avoiding playback interruptions entirely.
- **Best quality by default**: HLS streams automatically select the highest bandwidth variant. BiliBili uses the v1 API first for higher quality before falling back to v2.
- **FLV header caching**: Late-joining clients receive a cached FLV header before live data, enabling mid-stream connections without player errors.
- **No re-encoding**: Streams are forwarded as-is, keeping latency minimal.

## Development

- [Writing a Custom Extractor](docs/writing-extractor.md)

## License

[MIT](LICENSE.txt)
