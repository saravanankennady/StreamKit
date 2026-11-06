# Build from the repository root: docker build -f infrastructure/docker/server.Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg pkg
COPY server server
COPY api api
COPY database database
COPY egress egress
COPY ingress ingress
COPY sip sip
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /streamkit-server ./server/cmd/streamkit-server

FROM alpine:3.22
# The directory is created here so that a named volume mounted on it starts out owned by this user. The recording worker
# uses the same user ID.
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 streamkit && mkdir -p /var/lib/streamkit/recordings && chown -R streamkit /var/lib/streamkit
COPY --from=build /streamkit-server /usr/local/bin/streamkit-server
USER streamkit
# HTTP and WebSocket, ICE over TCP, ICE over UDP.
EXPOSE 7880/tcp 7881/tcp 7882/udp
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s CMD wget -q -O /dev/null http://127.0.0.1:7880/livez || exit 1
ENTRYPOINT ["streamkit-server"]
