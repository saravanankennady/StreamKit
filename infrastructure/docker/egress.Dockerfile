# The recording worker. Build from the repository root: docker build -f infrastructure/docker/egress.Dockerfile .
# Chrome comes from Google's package, which is built for amd64 only.
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /streamkit-egress ./egress/cmd/streamkit-egress

FROM debian:trixie-slim
ADD https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb /tmp/chrome.deb
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg fonts-noto-core /tmp/chrome.deb \
    && rm -rf /var/lib/apt/lists/* /tmp/chrome.deb \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin streamkit \
    && mkdir -p /var/lib/streamkit/recordings /var/lib/streamkit/work \
    && chown -R streamkit:streamkit /var/lib/streamkit
COPY --from=build /streamkit-egress /usr/local/bin/streamkit-egress
USER streamkit
ENTRYPOINT ["streamkit-egress"]
