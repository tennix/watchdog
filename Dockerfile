# builder
FROM golang:1.12 as builder

ENV GO111MODULE=on CGO_ENABLED=0

COPY . /go/src/github.com/pingcap/watchdog

WORKDIR /go/src/github.com/pingcap/watchdog

RUN go build

# executable
FROM alpine:3.8

COPY --from=builder /go/src/github.com/pingcap/watchdog/watchdog /watchdog

EXPOSE 9087

ENTRYPOINT ["/watchdog"]
