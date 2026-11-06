# The SIP worker. Build from the repository root: docker build -f infrastructure/docker/sip.Dockerfile .
# Ports and networking: see docs/deployment/sip.md.
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /streamkit-sip ./sip/cmd/streamkit-sip

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin streamkit
COPY --from=build /streamkit-sip /usr/local/bin/streamkit-sip
USER streamkit
EXPOSE 5060/udp 5060/tcp 5061/tcp
ENTRYPOINT ["streamkit-sip"]
