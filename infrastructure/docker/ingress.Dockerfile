# The ingest worker. Build from the repository root: docker build -f infrastructure/docker/ingress.Dockerfile .
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /streamkit-ingress ./ingress/cmd/streamkit-ingress

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin streamkit
COPY --from=build /streamkit-ingress /usr/local/bin/streamkit-ingress
USER streamkit
EXPOSE 1935/tcp
ENTRYPOINT ["streamkit-ingress"]
