FROM golang:alpine AS builder
RUN mkdir -p /app
WORKDIR /app
ADD . /app
RUN go build -o output/bin/lsf

FROM alpine:latest
COPY --from=builder /app/output/bin/lsf /bin
ENTRYPOINT ["/bin/lsf"]
