# Elnova StreamKit

Self-hosted real-time audio and video: an SFU, signalling, room and participant management, recording, ingress, phone
calls, voice AI agents, SDKs and a management dashboard, all running on your own machines.

**Status: early.** The server core works and is well tested. Production use is not advised yet: it has run on one machine
over loopback and in emulated networks, not on real servers, and no external security audit has been done. See
[what works](#what-works) and [what is still open](docs/project-status/incomplete-features.md).

## Try it

You need Go 1.26 or later.

```bash
make server
bin/streamkit-server --dev
```

That runs a media server on `ws://localhost:7880`, with the development credentials `devkey` and `secret`. Without a
database, there is no dashboard or REST API: rooms are created when someone joins, and the room API still works.

To add the control plane, start PostgreSQL and Redis and point the server at them:

```bash
docker compose up -d
export STREAMKIT_DATABASE_URL='postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable'
export STREAMKIT_MASTER_KEY=$(openssl rand -hex 32)    # keep this: it encrypts stored API secrets
bin/streamkit-server --dev
```

Running it on your own servers is in [self-hosting](docs/deployment/self-hosting.md).

## What works

Everything in this table has automated tests. "Both servers" means the same test also passes against a reference server,
which is how we check that the test itself is right.

| Area | Evidence |
| --- | --- |
| Audio and video between browsers, simulcast, screen share | Real Chrome, asserting decoded frames and changing pixels, on both servers |
| Mute, unpublish, subscriptions, permissions, data messages | Same |
| Reconnecting after a lost connection | Same |
| Room API (Twirp, JSON and protobuf) | Same, plus unit tests |
| Accounts, projects, encrypted API keys, webhooks, event history | Against real PostgreSQL |
| Tenant isolation, SSRF protection, rate limits | Against real PostgreSQL |
| Share links and the meeting app: guests join with no account, hosts and moderators | Real Chrome and real media |
| Several servers as one cluster, including a server dying | Two real processes with real clients |
| Recording and streaming out, RTMP, URL and WHIP in | Real Chrome and ffmpeg, checked by decoding the output. See [media services](docs/api/media-services.md) |
| Phone calls in and out, with audio checked by content | A real SIP phone, pjsua, with real Chrome, on loopback. No real trunk yet. See [SIP](docs/deployment/sip.md) |
| Voice AI agents with local speech and language models | Python and Node.js frameworks over real WebRTC. See [agents](docs/architecture/agents.md) |

The latest full regression and its results are in
[current-implementation-audit.md](docs/project-status/current-implementation-audit.md). Known failures and parts that
have not run are in [incomplete-features.md](docs/project-status/incomplete-features.md).

## Layout

| Path | What it is |
| --- | --- |
| `server/` | The media server: `sfu` (forwarding), `transport` (WebRTC), `room`, `participant`, `track`, `signaling`, `roomapi`, `routing` (clusters) |
| `api/` | The control plane: REST API, storage, webhooks, recording of events and usage |
| `database/` | Schema and migrations |
| `sdk-js/`, `sdk-react/` | Client SDKs |
| `sdks/server-go/`, `sdks/server-node/`, `sdks/server-python/` | Server SDKs: tokens, webhooks, rooms, recording, ingest |
| `cli/` | The `streamkit` command line |
| `dashboard/` | Management console |
| `meeting-app/` | Meeting page that guests reach through share links. Served at `/meet` |
| `egress/`, `ingress/` | Recording and streaming services |
| `sip/` | Connects phone calls to rooms. See [running it](docs/deployment/sip.md) and [its API](docs/api/sip.md) |
| `streamkit-agents/` | Voice AI agent frameworks for Python and Node.js |
| `proto/vendor/` | The wire protocol definitions, third-party and unmodified, under Apache-2.0 |
| `infrastructure/`, `monitoring/` | Docker, Compose, Helm, nginx, coturn, Prometheus, Grafana |
| `tests/` | Interoperability, end-to-end, media, network, SIP, agent and load tests |
| `docs/` | Architecture, deployment, API and project status |

## Testing

```bash
make test                # unit tests
make test-integration    # with PostgreSQL and Redis
make test-conformance    # the protocol's official client in real Chrome against this server
make test-sdks           # the server SDKs and the CLI against real servers
```

The conformance suite can run against any server:

```bash
TARGET_WS=ws://127.0.0.1:7880 node --test tests/interoperability/conformance.test.mjs
```

Suites that compare against a reference server run both by default. Point `REFERENCE_SERVER` at its binary, or set
`TARGETS=ours` to run against this server only.

## Author

Created by **Saravanan Kennady**, [kennadysaravanan@gmail.com](mailto:kennadysaravanan@gmail.com).

## Licensing

Copyright (c) 2026 Saravanan Kennady. All rights reserved: `LICENSE` grants no license yet, pending the owner's decision.
Third-party components keep their own licenses. See [NOTICE.md](NOTICE.md) and
[docs/architecture/licensing.md](docs/architecture/licensing.md).
