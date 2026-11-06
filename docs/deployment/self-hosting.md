# Self-hosting

Written for whoever will run this in production. Elnova StreamKit runs on your own machines with no dependency on any
cloud service. It has been tested on Ubuntu with Docker; the commands assume that.

**Status:** the stack below is ready to try, not yet battle-tested. See [What has been verified](#what-has-been-verified).

## How it fits together

```
                      browsers and apps
              |                                 |
         HTTPS, WSS                  UDP 7882, or TCP 7881
              v                                 |
          +-------+                             |
          | nginx |                             |
          +-------+                             |
         /    |    \                            |
 dashboard  meeting  server  <------------------+
                     /    \
              PostgreSQL  Redis
```

Signalling and APIs go through nginx. **Media does not**: clients send it straight to the server that hosts their room.
That has two consequences you must plan for: every media server needs its own public IP, and a load balancer can
only ever carry signalling.

## Requirements

- Ubuntu 22.04 or later, Docker Engine and the Compose plugin
- A public IP address and a DNS name pointing at it
- A TLS certificate for that name. Browsers refuse camera access on insecure origins, so this is not optional
- SMTP credentials, if you want email verification and password reset to work

No capacity figure is stated here because none has been measured for your hardware. Run the load test in
`tests/load/` before sizing. See [Load testing](#load-testing).

## Ports and firewall

Open these on the host, and in any cloud security group:

| Port | Protocol | Who connects | Purpose |
| --- | --- | --- | --- |
| 80 | TCP | Everyone | Redirects to HTTPS |
| 443 | TCP | Everyone | Dashboard, meeting app, API, signalling |
| 7882 | UDP | Everyone | Media. **The important one** |
| 7881 | TCP | Everyone | Media fallback for networks that block UDP |
| 1935 | TCP | Encoders such as OBS | RTMP ingest. Close it if you do not use RTMP |
| 3478 | TCP and UDP | Everyone | STUN and TURN |
| 5349 | TCP | Everyone | TURN over TLS, for the strictest networks |
| 49160-49800 | UDP | Everyone | TURN relay ports |

On Ubuntu:

```bash
sudo ufw allow 80,443,7881,1935,3478,5349/tcp
sudo ufw allow 7882,3478/udp
sudo ufw allow 49160:49800/udp
```

Between servers, in a cluster: nodes reach each other on 7880/TCP. **Never expose 7880 publicly**, nor PostgreSQL (5432)
or Redis (6379). Docker publishes ports past `ufw`, which is why the Compose file publishes only what is listed above.

## Recording and ingest

The stack includes two workers, which find their work in the database. `egress` records rooms and streams them out, and
`ingress` accepts RTMP and pulls from URLs. WHIP needs neither: publishers connect straight to `server`. A third,
`sip`, connects phone calls to rooms. It is off unless you ask for it: see [Running the SIP service](sip.md).

- **They join rooms like any client,** so their media goes to `STREAMKIT_PUBLIC_IP` like everyone else's. That address has to
  work from inside a container on the host. On a machine that has the address on its own interface, it does. Behind a
  cloud provider's 1:1 NAT, check that the provider lets a machine reach its own public address.
- **One recording takes about 0.7 of a core and 800 MB** for two people with easy video, and more with real cameras. See
  [the measurements](../api/media-services.md#running-the-workers). The limit per worker is `media.egress.max_jobs`, and
  `docker compose up -d --scale egress=2` adds workers.
- **Recordings are kept in the `recordings-data` volume,** which `server` and `egress` share. For several hosts, or to
  keep them off the local disk, set `media.storage.kind: s3` in the `server` config in `production.yml`.
- **Chrome's own sandbox is on in the container,** using the seccomp profile in `infrastructure/docker/chrome-seccomp.json`, which
  lets it make the namespaces it needs. On a host where that does not work, the worker says so when it starts. Recording
  means decoding media from anyone in a room, so keep the image up to date to get Chrome's security fixes: rebuild it with
  `docker compose build --pull --no-cache egress`. See
  [what a media process can reach](../api/media-services.md#what-a-media-process-can-reach).
- **Workers log in to the database as `streamkit_worker`,** with `STREAMKIT_WORKER_DB_PASSWORD`, which is not a superuser and can only
  reach the job tables. They are on a network of their own with the server and the database.
- **The workers' root filesystems are read-only,** so a compromised Chrome or ffmpeg cannot change anything in the image. They
  write to `/tmp`, shared memory and their volumes. Compose cannot copy an inline config into a read-only root, so `production.yml`
  gives them the same configuration as the server in the variable `STREAMKIT_CONFIG_YAML`. There is nothing extra to do: to change
  the configuration, edit `x-config` at the top of `production.yml`, and every service gets it. Keep secrets out of it. They
  belong in `.env`, and reach each service by name.
- **Several projects on one installation** should set `media.egress.max_jobs_per_project` and
  `media.ingress.max_sessions_per_project`, or one project can take every slot there is.

`GET /api/v1/servers` shows each worker, and whether it is healthy. A worker reports itself unhealthy when it could not do a
job, such as with a full disk or storage it cannot reach, and takes no new work until that is fixed.

## Public IP

The server advertises one address to clients: `STREAMKIT_PUBLIC_IP`. Get this wrong and everything looks healthy,
signalling connects, and then no media ever flows. It is the most common self-hosting failure.

- On a VM with a public IP, or behind 1:1 NAT as on AWS, GCP and Azure: set it to the public IP. Traffic must arrive at
  the host on the same port numbers.
- Behind a home router or other port-mapping NAT: forward 7881 and 7882 with the same numbers externally.

## TLS certificates

With Let's Encrypt, before starting the stack:

```bash
sudo apt install certbot
sudo certbot certonly --standalone -d rtc.example.com
```

Point `STREAMKIT_TLS_DIR` at `/etc/letsencrypt/live/rtc.example.com`. Renewal needs nginx and coturn restarted to pick up the
new files; add that to a certbot deploy hook. Those directories hold symlinks into `../../archive`, so mount
`/etc/letsencrypt` if the links do not resolve in the containers.

## Install

```bash
cp infrastructure/compose/.env.example .env
# Fill in .env. Generate each secret with: openssl rand -hex 32
docker compose -f infrastructure/compose/production.yml --env-file .env up -d --build
```

Then open `https://rtc.example.com` and register. **The first account becomes the platform administrator**, so do this
straight away, before anyone else can reach the page.

Back up `STREAMKIT_MASTER_KEY` separately from your database backups. It encrypts API secrets: without it they are
unreadable, and with it and a database dump, an attacker has them all.

## Accounts

Registration is closed by default: `control_plane.allow_registration`, or `STREAMKIT_ALLOW_REGISTRATION`, is false unless you
set it. An account can make projects, keys and rooms, and start meetings, so a server open to the internet that takes accounts
from anyone is running rooms for anyone.

The first account can always register, whatever this says, and that is what makes closed a safe default. That account
administers the installation, and nothing else can make it, so an installation with no accounts takes one. From then on,
the page refuses with `registration_closed`.

There is no way to invite someone yet. To add a person, set `STREAMKIT_ALLOW_REGISTRATION=true` in `.env`, restart the server, let
them register, and set it back. Each account gets its own organization. With `--dev`, registration is open unless you say otherwise.

**A server you are upgrading closes too.** If you rely on people registering, set `STREAMKIT_ALLOW_REGISTRATION=true` first.

A project can have 1000 meetings open at once, meaning not ended, with a link that still works. `control_plane.max_meetings_per_project`
changes it, and a negative number sets no limit.

## Keeping tokens out of logs

A browser can only put an access token in the address of a WebSocket, so `/rtc` has a live token in every request line, and reset and
verification links carry theirs the same way. Anything that logs addresses in front of this server should leave out the query
string. The nginx config in this repository does: it logs paths only, and no Referer, which is the same address again for what a page
loads next. On `/rtc` it also keeps error messages to `crit`, since nginx quotes the whole request line in those, so a server that
cannot be reached shows there as a 502 and no more. Another proxy needs the same treatment.

The Helm chart gives `/rtc` an Ingress of its own with its access log off, which is as much as an Ingress can say. The ingress-nginx
controller's error log still quotes the request line, at its own level: set `error-log-level: crit` in its ConfigMap if you
keep those logs, or put up with that. This has not been run on a cluster.

## What is kept, and for how long

Rows go in a pass that runs while the server does. These are not settings.

| What | How long |
| --- | --- |
| Audit log | 1 year |
| Event log, webhook deliveries | 30 days |
| Usage figures | 400 days |
| Someone's row in a meeting, with the name they gave | 30 days after they were last active |
| The same for someone who never got in: a place in the waiting room, or a token never used | 1 day |
| A share link, after it expired or was turned off | 30 days, and until nothing points at it |
| A meeting, after it started | 30 days, and until no link or member points at it and its room has closed |

## TURN

Some users sit on networks that block direct media, and TURN relays for them. It uses real bandwidth, so watch it.

The Compose file runs coturn with `STREAMKIT_TURN_SECRET`, and the server mints each client a credential that expires after a
day. Nothing else is needed. A project can list its own servers under **Developer settings**, and those are used in place of
the server's `ice_servers`, while the minted TURN credential is still added.

coturn is told to refuse relaying to private networks. Without that, anyone holding TURN credentials could reach your internal
services through it.

## Scaling out

Add servers by pointing them at the same PostgreSQL and Redis:

- Each needs its own `STREAMKIT_PUBLIC_IP`, its own open media ports, and `STREAMKIT_NODE_URL` set to an address other nodes can reach
  it at directly, not through a load balancer.
- Put all nodes behind your load balancer for signalling. Any node accepts any request and proxies it to the node hosting
  the room. Sticky sessions are not needed.
- A room lives on one node. It cannot span nodes, so one room's size is limited by one machine. If a node has been out of touch
  with Redis long enough for another to take over one of its rooms, its copy closes when it gets back, and the clients on it
  connect again and reach the real one.
- Nodes sign the requests they pass to each other, with a key made from `STREAMKIT_MASTER_KEY`, or without a control plane, from
  the static API keys. There is nothing to set up, but the nodes must all have the same one. A client sending
  `X-StreamKit-Forwarded-By` itself gets nowhere. The nginx config in this repository removes it anyway, and another load
  balancer can do the same.
- Ending a meeting or removing someone answers 503 if the node cannot tell who holds the room, as when it cannot reach Redis.
  Nothing has changed then, and it is safe to try again.
- Before maintenance on a node, drain it from the **Servers** page. It stops taking new rooms and carries on with what it has.
- If a node dies, its clients reconnect and the room restarts on another node. Calls are interrupted, and how long depends on
  client timeouts: expect tens of seconds. Clients must connect through the load balancer's address for this to work.

Kubernetes: use `infrastructure/helm/elnova-streamkit`. Media pods need host networking, one per machine, because clients
must reach each directly with UDP. The chart does this with a DaemonSet. Provide PostgreSQL and Redis separately, and a Secret
holding `STREAMKIT_MASTER_KEY`, `STREAMKIT_DATABASE_URL`, `STREAMKIT_REDIS_URL`, `STREAMKIT_METRICS_TOKEN` and `STREAMKIT_NODE_IP`.

## Monitoring

Prometheus and Grafana come with the Compose file. Grafana is at `/grafana/`, with an overview dashboard provisioned.
Alert rules are in `monitoring/prometheus/alerts.yml`, but nothing delivers them until you add Alertmanager.

`/metrics` needs `Authorization: Bearer $STREAMKIT_METRICS_TOKEN`. Health endpoints need no credentials:

| Endpoint | Meaning | Use it for |
| --- | --- | --- |
| `/livez` | The process is running | Restart decisions |
| `/readyz` | The database and Redis answer, and the node is not draining | Load balancer membership |
| `/healthz` | Alias of `/livez` | |

Set `STREAMKIT_OTLP_ENDPOINT` to send OpenTelemetry traces of API and signalling requests to a collector, such as
`http://otel-collector:4318`. Logs are JSON on stdout.

## Backups

- **PostgreSQL:** the only stateful store that matters. `pg_dump` on a schedule. Room state in Redis and in memory rebuilds itself.
- **`STREAMKIT_MASTER_KEY`:** keep it in a password manager or secret store, apart from the database dumps.

## Upgrades

Schema changes run automatically at startup, and nodes take turns. With several nodes, upgrade one at a time: drain, wait
for its rooms to empty, replace, resume. Nodes sign what they pass to each other, so one that has not been upgraded cannot prove
itself to one that has, and the request is routed afresh instead. That works, with one more hop.

## Troubleshooting

**Clients connect, then video never appears, or `connect()` fails after 15 seconds.** ICE failed. Check `STREAMKIT_PUBLIC_IP`, and
that UDP 7882 is open from outside: from another network, run `nc -u -z rtc.example.com 7882`, though only a packet capture on
the server proves it. `chrome://webrtc-internals` shows which candidates were tried.

**A share link works on your computer, but not for anyone else.** Links are built from `public_dashboard_url`, and guests connect to
`public_ws_url`. Both must be addresses other people can reach, and the page must be HTTPS: browsers only offer cameras and microphones
on secure pages, so over plain HTTP a guest can only watch and listen. `STREAMKIT_PUBLIC_IP` must be right too, or the page loads and no media
flows.

**Works on your network, fails for some users.** They are probably on a network that blocks UDP. Check 7881/TCP, and set up TURN.

**`invalid API key` right after rotating.** Keys are cached for up to 30 seconds on other nodes.

**Everything is down after a reboot.** Check that `STREAMKIT_MASTER_KEY` did not change. Secrets encrypted under the old key cannot be read.

**A shared screen moves unevenly on a network with a lot of jitter.** `rtc.screen_share_max_playout_delay_ms` limits how long a viewer's browser may hold
frames of a shared screen, and is 100 by default. A network with more jitter than the limit shows it as uneven motion, so raise it for such a network. A
higher limit adds that much delay. `0` sets no limit, which leaves the choice to the browser.

## What has been verified

| Verified | How |
| --- | --- |
| Server, control plane, clustering | Automated tests, against real PostgreSQL and Redis, with real Chrome clients |
| Failover when a node dies | Automated test with two processes on one machine |
| Prometheus alert rules | Checked with `promtool` |
| Compose file | Parses, and refuses to start with secrets missing |
| nginx and TLS | Automated test, `tests/media/compose.test.mjs`: a call, recording downloads with Range, a WHIP publisher and the live monitor, through the real template with a self-signed certificate, on one machine. The dashboard, meeting app and Grafana behind it were stand-ins, so what goes to them through nginx has not been run |
| coturn, Grafana, Helm | **Not yet run.** Configuration review only |
| OpenTelemetry export | Span naming is unit tested. Export to a real collector has not been run |
| TURN credentials | Format unit tested against a value computed with `openssl`. Not tried against a real coturn |
| Two machines on a real network | **Not yet run** |
| Share links and the meeting page | Automated tests in real Chrome, on one machine. Not yet tried from another device, or through nginx |
| The server and worker images, and the workers' place in the compose file | Automated test, `tests/media/compose.test.mjs`: built, run with `production.yml`'s wiring on one machine, and recording and ingesting real media through the published ports, with Chrome's sandbox on and a read-only root on the workers. Without TURN |
| Dashboard and meeting images | Not yet built in this repository. |

Treat this as a candidate, and run it on a staging host before relying on it.

## Load testing

See `tests/load/README.md`. Measure on your own hardware, with your own typical room size. Published numbers from other
machines will not tell you what yours can do.
