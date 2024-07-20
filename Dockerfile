FROM golang:alpine AS builder
ARG VERSION="0.0.0"
ARG BUILD_TIME="Thu Jan 01 1970 00:00:00 GMT+0000"
ARG SHA="e5fa44f2b31c1fb553b6021e7360d07d5d91ff5e"

ENV GOPROXY="https://goproxy.cn,direct"

COPY . /go/src/github.com/nv4d1k/live-stream-forwarder
WORKDIR /go/src/github.com/nv4d1k/live-stream-forwarder

RUN set -Eeux && \
    go mod download && \
    go mod verify

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-extldflags \"-static\" -X 'github.com/nv4d1k/live-stream-forwarder/global.Version=${VERSION}-docker' -X 'github.com/nv4d1k/live-stream-forwarder/global.BuildTime=${BUILD_TIME}' -X github.com/nv4d1k/live-stream-forwarder/global.GitCommit=${SHA}" \
    -o /bin/lsf
RUN go test -cover -v ./...

FROM scratch
COPY --from=builder /bin/lsf /
ENTRYPOINT ["/lsf"]
