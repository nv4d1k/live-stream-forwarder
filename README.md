# Live Stream Forwarder
## Overview
# Live Stream Forwarder (formly streamlink-go) is a tool that converts website live streams into local live broadcast services for video players (e.g. VLC or PotPlayer) directly access.
## Supported Live Streaming Platform
| Platform                                         | URL token |
|--------------------------------------------------|-----------|
| [Douyu](https://www.douyu.com "douyu.com")       | douyu     |
| [Huya](https://www.huya.com "huya.com")          | huya      |
| [Twitch](https://twitch.tv "Twitch")             | twitch    |
| [Bilibili](https://live.bilibili.com "BiliBili") | bilibili  |
| [Douyin](https://live.douyin.com "Douyin")       | douyin    |
## Installing
Just download at [releases](https://github.com/nv4d1k/live-stream-forwarder/releases "releases") page and decompressing it to anywhere you wanted.
## Usage
Start the service listening on ip address 127.0.0.1 and a random port by default. e.g.

    lsf
Start. the service listening on specified address or port. e.g.

    lsf -l <address> -p <port>
or

    lsf --listen-address <address> --listen-port <port>
Start the service with debug mode. e.g.

    lsf --log-level 6
Start the service with storing logs on file. e.g.

    lsf --log-file example.log
Start the service with http proxy. e.g.

    lsf --proxy http://<username>:<password>@<address>:<port>
Open the stream on video player. e.g.

    http://<address>:<port>/<platform url token>/<room id>
Open the stream on video player with http proxy. e.g.

    http://<address>:<port>/<platform url token>/<room id>?proxy=http://<username>:<password>@<address>:<port>
## License
See [LICENSE.txt](LICENSE.txt)