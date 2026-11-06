# Running the SIP service

`streamkit-sip` connects phone calls to rooms. It is a separate program from the server, and you can run as many as you need on other
machines. It reads the same configuration file as the server, and needs the same database and master key. It talks to the server only
through the database, plus an ordinary client connection for each call's room.

**What this has been tested with.** Everything below was tested on one machine over loopback, against
[pjsua](https://github.com/pjsip/pjproject) as an independent phone. It has never carried a call from a real trunk or a real phone, or
crossed a real network or a NAT. See [What has not been tested](#what-has-not-been-tested).

## Ports

| Port | Protocol | Who needs to reach it | Setting | Default |
| --- | --- | --- | --- | --- |
| SIP | UDP | Your carrier | `media.sip.udp_addr` | `:5060` |
| SIP | TCP | Your carrier | `media.sip.tcp_addr` | `:5060` |
| SIP over TLS | TCP | Your carrier, if it uses TLS | `media.sip.tls_addr` | off |
| RTP and RTCP | UDP | Your carrier's media servers | `media.sip.rtp_port_min`, `rtp_port_max` | `10000` to `20000` |

A call takes two media ports, an even one for RTP and the next for RTCP. So the range sets a limit on calls: 10,000 ports is 5,000 calls, and
the 200 in the Compose file is 100.

The worker also opens these connections out:

- To your carrier, for outbound calls: SIP to the trunk's address, and RTP to wherever its SDP says.
- To the server: `media.ws_url`, plus WebRTC to the server's RTC ports, like any participant.
- To PostgreSQL.

## Firewall

**Allow your carrier's addresses, and nobody else, to reach the SIP ports.** Carriers publish these. A SIP port open to the internet gets
scanned within minutes, and the checks in this service are the second line of defence, not the first. Set `off` as the address of any
transport you do not use.

Open the RTP range to your carrier's media addresses if you can. Some carriers use a very wide range for media, and if yours does, open it to
everyone: a packet from anywhere other than a call's peer is dropped, whatever port it reaches.

## What you have to set

```yaml
media:
  ws_url: wss://rtc.example.com   # How workers reach rooms.
  sip:
    # What peers reach this worker at. It goes in Contact headers, and requests in a call come straight back to it, so it has to reach
    # this exact worker. Not a load balancer.
    public_host: sip1.example.com
    # What goes in SDP. RTP is sent here, so it has to be this machine's public IP. Required if public_host is a name.
    media_ip: 203.0.113.10
```

Everything else has a default, listed in `server/config/config.go` under `SIPConf`.

| Setting | Default | What it does |
| --- | --- | --- |
| `max_calls` | 25 per CPU | Calls on one worker. This default is not a measured capacity: measure yours. |
| `max_calls_per_project` | 50 | Calls one project can have at once, across all workers, both directions. Read by both the server and the worker. |
| `max_outbound_per_minute` | 120 | Outbound calls one project can start in a minute. Read by the server, per server. |
| `invites_per_second`, `invite_burst` | 20, 40 | Messages one address may send. An address on a trunk's allow-list gets 20 times this. |
| `ban_after`, `ban_seconds` | 20, 600 | Refused requests in a minute before an address is ignored, and for how long. Negative `ban_seconds` turns this off. Addresses on an allow-list are never banned. |
| `silent_rejects` | off | Give no answer to anything from an address that is on no allow-list, rather than an error. Turn this on when you can: a scanner learns nothing. It makes problems harder to diagnose, because a misconfigured carrier gets silence. |
| `allow_unauthenticated_trunks` | off | Allow inbound trunks with neither an allow-list nor a password. Only for a deployment with something else in front doing that checking. |
| `ringing_seconds` | 180 | How long a call rings before giving up, unless a trunk or request says. |
| `media_timeout_seconds` | 15 | Ends a call with no RTP for this long (30 before the first packet). Not while it is on hold. |
| `max_call_seconds` | one day | Ends any call this long. |
| `lease_seconds` | 30 | How long before anyone can mark a silent worker's calls failed. |
| `drain_seconds` | 60 | On SIGTERM, how long calls get to end by themselves before being hung up. |
| `media.allow_private_addresses` | off | Allow outbound trunks at private addresses. Leave this off, or any project can use your workers to reach inside your network. |

## NAT

The simple setup is a public IP on the machine, or a one-to-one NAT that forwards every port in the table above to it, with `public_host` and
`media_ip` set to that public address.

A phone or carrier behind its own NAT is handled like this: RTP is sent back to wherever the first packet from it came from, as long as that is
its signalling address or the address in its SDP. Requests in a call go back to the address its signalling came from if its Contact header
names a private address.

**None of this has been run through a real NAT.** It has been tested with a port number that is wrong in the SDP, on loopback.

### Docker

The Compose entry publishes port 5060 and 200 RTP ports, and is off unless you ask for it:

```
docker compose -f infrastructure/compose/production.yml --env-file .env --profile sip up -d
```

- Allow-lists depend on seeing the caller's real address. Check that yours does: with `log_level: debug`, the worker logs the source of each
  request it refuses. Some Docker setups, and every userland proxy, show the gateway's address instead.
- For more calls than that range allows, use `network_mode: host` and give it a wider one. Docker is slow at publishing a large range.
- One published port 5060 means one worker per machine in Compose. To add workers, run them on other machines.

## More than one worker

Workers share nothing but the database. Run another by starting it with the same file and its own `public_host`.

- **Outbound:** any worker with room takes the next queued call.
- **Inbound:** a carrier reaches a worker by whatever address you give it, so give it each worker's address, as failover or round robin if it
  can do that, or use DNS. Each worker has to be reachable at its own `public_host`, since requests in a call return to it. There is no
  SIP-aware load balancer here, and the worker does not add Record-Route.
- **Draining:** `POST /api/v1/servers/{id}/drain`, or SIGTERM. A draining worker answers new INVITEs with 503 and takes no outbound calls.
- **If a worker dies:** its calls are marked failed when their leases run out, by any other worker or by the server. Their participants leave the
  room at once, because their connections have dropped. The phone is **not** told, because the dialog state went with the worker. It hears silence
  until it gives up, or until its carrier does. A restart does not pick those calls up again.

## How inbound calls are checked

In this order, stopping at the first failure. Everything before the database lookup is cheap, so a flood costs little.

1. **The guard.** Banned or over-budget addresses are dropped before the message is parsed.
2. **Known scanning tools.** Dropped with no answer, by User-Agent. This is only noise reduction: it is trivially avoided.
3. **Capacity.** 503 with Retry-After when the worker is full or stopping.
4. **Trunk.** The call must match a trunk by called number, source address and calling number. **No match means 403.** A match on a trunk with
   neither an allow-list nor a password also means 403. See [Differences from the reference](#differences-from-the-reference).
5. **Password.** If the trunk has one: 407 with a challenge, then 403 if the answer is wrong. The challenge is signed and holds no state on the
   worker, is bound to that call and trunk, and is good for five minutes. An answer works once. Digest is MD5, because that is what carriers do.
6. **Dispatch rule.** 404 if none applies.
7. **Media.** 488 if nothing can be agreed.
8. **Project limit.** 503 with Retry-After.

There is no relay of any kind. An INVITE only ever leads to a room. A REFER from a peer is refused. Outbound calls come only through the API, from
a token with the `sip.call` grant, through a trunk in that token's project or a trunk the request gives itself, to a public address unless you
allow otherwise.

### Trunks in a shared installation

An INVITE names no project, so trunks are matched across all of them. A trunk for a number beats a trunk for any number, and two that could both
claim a call are refused when the second is created, with an error naming the first one's ID. **That means one project can tell that another has a
number**, and can claim a number first. If your projects do not trust each other, have an administrator set up inbound trunks.

## Credentials

Trunk passwords and user names, and dispatch rule PINs, are sealed with the master key before they reach the database, and no API returns them.

- `has_credentials` on the REST routes says whether a trunk has any.
- A PIN shows as `****`.
- Reading a trunk or rule and sending it back keeps what it has. To remove a trunk's credentials, send an update that sets them empty.

Losing the master key loses these, as with API secrets.

## Media and codecs

| | Status |
| --- | --- |
| G.711 (PCMU, PCMA) | Yes, at 8 kHz. |
| Opus towards the room | Yes: 32 kbit/s mono, at 48 kHz, through a resampler. |
| G.722, AMR-WB, Opus towards the phone | No. An offer with only these gets 488 `No Acceptable Codec`. |
| RFC 4733 DTMF | Both ways. Audio stops during a digit going out. |
| SIP INFO DTMF | Coming in, from `application/dtmf-relay`. Going out is RFC 4733 only. |
| SRTP | SDES only (`AES_CM_128_HMAC_SHA1_80` and `_32`), not DTLS-SRTP. Set `media_encryption` on a trunk to allow or require it. |
| Early media | Not forwarded. A carrier's ringback in a 183 is not heard in the room: use a dial tone. |
| Late offer (INVITE with no SDP) | No: 488. |
| 100rel, session timers | Not offered. A `Require` header gets 420. |
| Jitter buffer | Only a small one, towards the phone. Packets that arrive late from the phone are dropped, not reordered. |

**SDES keys travel in the SDP.** They are only as private as the signalling, so use TLS with SRTP. SRTP with signalling in the clear keeps nobody out
who can read the signalling.

**PIN prompts are tones,** not speech: a short beep to ask, a fast one for a wrong entry. Nothing here ships recorded voices, because which voice and
what language are yours to choose. The dial tones are synthesized, from the national cadences.

## TLS

```yaml
media:
  sip:
    tls_addr: ":5061"
    tls_cert_file: /etc/streamkit/sip-cert.pem
    tls_key_file: /etc/streamkit/sip-key.pem
    # Only for a carrier with a private certificate authority. Outbound certificates are always checked against the trunk's name.
    # tls_ca_file: /etc/streamkit/carrier-ca.pem
```

A trunk with transport TLS calls out over it. Inbound, a peer connects to this port. Client certificates are not asked for: use an allow-list or a
password as usual. The worker reads its certificate at start, so restart it to change one.

## Monitoring

`GET /api/v1/servers` shows each worker with `stats.kind` of `sip_worker`, its calls and its health. `GET /api/v1/sip/calls` is the record of every
call: who, which trunk and rule, how long, and how it ended. A call's history is kept until something deletes it; nothing does so yet.

Logs are JSON on standard output. They hold call IDs and source addresses, no passwords or digest answers, and no phone numbers from this service's own
log lines, but `log_level: debug` also shows what the SIP stack logs.

## Differences from the reference

The API and the names are the reference's, so that its SDKs and CLI work. These differ, on purpose.

| | Reference | Here | Why |
| --- | --- | --- | --- |
| A call matching no trunk | Goes on to dispatch rules | 403 | A dispatch rule with no trunk would otherwise take a call from anyone. |
| Trunk with only numbers | Allowed | Refused on create, and on call | A phone number says who is being called, not who is calling. |
| Source address that cannot be read, or an allow-list of only invalid entries | Allowed | Refused | These fail open in the reference. |
| Host name in an allow-list | Matches the From header | Only counts when the trunk also has a password | A From header can say anything. |
| An allow-list of `/0`, or close to it | Allowed | Refused | It is not a list. |
| Wrong digest answer | 401 with no challenge | 403 | RFC 3261 says a 401 must carry a challenge. |
| Digest nonce | Remembered per call | Signed, stateless, bound to the call and trunk, usable once | A flood of challenges costs no memory. |
| Credentials and PINs in API responses | Returned | Never returned | Secrets stay sealed. Reading and sending back keeps them. |
| Call history | None: the state updates go nowhere | `GET /api/v1/sip/calls`, kept in the database | |
| Control plane | psrpc on Redis | The database | A separate worker needs nothing else. So the reference's bridge cannot run against this server, and this one cannot run against the reference's. |
| List of a project's calls | None | REST only | There is no Twirp method for this. |

Not implemented from the reference: G.722, the `sip_request_uri`, `sip_from_header` and `sip_to_header` request fields (accepted and ignored),
automatic header mapping beyond a short list, and `destination_country`, `krisp_enabled` and `room_preset`, which are stored and do nothing.

## Testing

The tests in `tests/sip/` make real calls, over real SIP and RTP sockets, on loopback. Audio is checked by its content: a tone goes in one end, and what comes
out of a real decoder at the other end has to be that tone.

| File | What it does | Needs |
| --- | --- | --- |
| `api.test.mjs` | The API, through the official Node client, Python client and CLI, on this server and the reference | |
| `phones.test.mjs` | Phones through a room: audio both ways, digits, transfers, TCP, TLS, SRTP, PINs, a worker dying or stopping, two workers | pjsua |
| `calls.test.mjs` | Phones and someone in a room with the official client library: a tone from Chrome to a phone, and back | pjsua, Chrome |
| `abuse.test.mjs` | Scanners, floods, malformed messages, password guessing, relaying, limits | pjsua |

`phones` and `calls` run the same scenarios on the reference where they can. `abuse` is only about this service, since those are mostly its own decisions.

pjsua is GPL-2.0 and is not shipped with anything: it is a separate test tool, run as a separate process. To build it, with OpenSSL headers installed:

```
curl -L https://github.com/pjsip/pjproject/archive/refs/tags/2.15.1.tar.gz | tar xz
cd pjproject-2.15.1
printf '#define PJSUA_MAX_CALLS 32\n#define PJ_HAS_SSL_SOCK 1\n#define PJMEDIA_HAS_SRTP 1\n' > pjlib/include/pj/config_site.h
./configure --with-ssl --disable-video --disable-sound --disable-opus --disable-libwebrtc
make dep && make
```

Point `PJSUA` at the binary it makes under `pjsip-apps/bin/`. The tests also need the development PostgreSQL. Then:

```
cd tests/sip && npm ci && npm test
```

To run the same scenarios against the reference, build its bridge as the image `elnova-sip-oracle:dev`, with `docker build -t elnova-sip-oracle:dev -f build/sip/Dockerfile .` in
`reference/sip`, and have the reference server's binary where `tests/sip/lib.mjs` looks for it. They also use the development Redis. The tests run without those if they are
missing, and say that they skipped them. `SIP_SKIP_REFERENCE=1` runs only this service's half, and `SIP_ONLY_REFERENCE=1` only the reference's, in `phones` and `calls`. `LK` points
at a build of the official CLI, and `SIP_TEST_PYTHON` at a Python with `livekit-api`. `SIP_RACE=1` builds everything under the race detector.

## Why these libraries

| Need | Chose | Because | Instead of |
| --- | --- | --- | --- |
| SIP | [emiago/sipgo](https://github.com/emiago/sipgo) | BSD-2-Clause, actively maintained (release in the week this was written), and pure Go. Transactions, UDP, TCP and TLS. It is the upstream of the fork the reference uses. | The reference's fork, which carries changes for its own needs, and gosip, which has had far less work lately. |
| Opus | [thesyncim/gopus](https://github.com/thesyncim/gopus) | BSD-3-Clause, written in plain Go, with no C and hardly any unsafe code, and checked by its authors against libopus 1.6.1 with differential fuzzing. Every other worker here builds with `CGO_ENABLED=0`, and this keeps it that way. These packets come from whoever is in a room, so memory safety counts. On the development machine, once, on an i7-14700HX, a 20 ms frame costs about 0.14 ms to encode and 0.035 ms to decode. That is not a capacity figure. | libopus through cgo, which would need a C toolchain and headers to build the module. `kazzmir/opus-go`, which is libopus transpiled to Go and holds pointers as integers: **it was tried, and it crashed the worker in the first real call**, when a goroutine's stack moved in the middle of a decode. `pion/opus`, which cannot encode. gopus v0.2, which needs Go 1.27, ahead of this repository. |
| Digest | [icholy/digest](https://github.com/icholy/digest) | MIT, small, and already pulled in by sipgo. Only for parsing: the check is here. | |
| RTP, SRTP, SDP, WebRTC | Pion | Already used throughout this project. | |

gopus is young, and it has one main author, and its module is a 77 MB download because of its test data, which every image build fetches. So it sits behind
`sip/media/opus.go`, which is under 100 lines, and a panic in it is caught there rather than taking down the worker. It can be swapped for libopus without touching
anything else.

## What has not been tested

Each of these needs something that is not available on the machine where this was built.

- **A real trunk, and real carrier interop.** Needs a trunk from a carrier. No carrier has been tested, and none should be taken as supported.
  The headers mapped for Twilio, Telnyx and Amazon are copied from what the reference maps, and have never seen those carriers.
- **A real phone.**
- **A real network, and NAT in any form.** All results are from loopback.
- **The image, and Docker networking.** The image has not been built. The Compose file has only been parsed, and its entry has not carried a call.
- **Capacity.** Nothing has measured how many calls a worker can carry. `max_calls` is a limit, not a promise.
- **Attack from the internet.** The abuse tests are on loopback, from one machine, in a test's idea of what a scanner does.
