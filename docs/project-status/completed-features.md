# Completed features

Built and verified by automated tests. "Both servers" means the same test also passes against the reference server. A
feature is listed only with its evidence. Unless a row says otherwise, everything ran on one machine over loopback, so none
of it says how the feature behaves on a real network. The regression results are in
[current-implementation-audit.md](current-implementation-audit.md).

## Media server

| Feature | Evidence |
| --- | --- |
| Audio and video between browsers, asserted by decoded frames and changing pixels | Both servers |
| Late join; three-way calls; screen share | Both servers |
| VP8 simulcast, with layer switching in both directions | Both servers |
| VP9, H264 and AV1 publish and decode; H264 simulcast in three layers | Both servers. `tests/interoperability/transport/codec.test.mjs` 14 of 14 |
| Sender reports from the publisher's clock, for A/V sync | Both servers. `avsync.test.mjs` 14 of 14; report error 0.03 ms |
| Data tracks | Both servers. `datatrack.test.mjs` 24 of 24 |
| `/rtc/v1`, the single peer connection protocol, on by default | Conformance 41 of 41 in v1 and dual mode on both servers, with a check that fails on a silent fallback. Chrome only. `rtc.single_peer_connection: false` restores the 404 |
| Dynacast, adaptive stream | Both servers |
| Mute, unpublish and republish; muted audio is not forwarded | Both servers |
| Auto-subscribe off, manual subscribe, subscription permissions | Both servers |
| Reliable and lossy data, topics, destinations; exactly-once delivery across a resume | Both servers |
| Resume; failed resume leading to rejoin; duplicate identity | Both servers |
| Token grants: publish, subscribe, sources, own metadata, data following publish, room configuration | Both servers |
| Room isolation | Both servers |
| Active speakers, connection quality | Both servers. Own scoring model; a viewer is not marked down for loss before the server |
| Room API: 11 methods, JSON | Both servers. Protobuf is unit tested |
| Firefox publishing and viewing | `tests/network`, 5 of 5. Firefox screen share not tested |
| Behaviour under loss, round trip, outages, UDP blocked, TCP fallback, TURN with real coturn, signalling cut, network switch | `tests/network`, emulated with `tc netem` and `iptables` in containers. Ours met every check in 34 of 43 cases, the reference in 26. No real network, router or NAT. See [network-test-results.md](../operations/network-test-results.md) |
| Multi-node routing, including a node dying; signed hop between nodes; one owner per room | Real processes on one machine, `tests/cluster` and `multinode.test.mjs` |

## Control plane and dashboard

| Feature | Evidence |
| --- | --- |
| Accounts, sessions, email verification, password reset, `allow_registration` | Real PostgreSQL |
| Organizations (one per signup), projects, encrypted API keys, rotation | Real PostgreSQL |
| Tenant isolation, roles on routes, CSRF and SSRF protection, login rate limits | Real PostgreSQL |
| Access tokens reach only the routes whose grant they carry | `TestEveryRouteSaysWhatATokenGets` walks the whole route table |
| Signalling and data rate limits | Real server processes, `tests/cluster` and `server/participant` |
| Share links: make, list and turn off; guests join with no account; host links | Real Chrome and real media through our server |
| Webhooks: signed, retried, in order, delivery history | Real PostgreSQL. Deliveries pass the official Node and Python receivers |
| Event log, audit log, usage accounting, live metrics | Real PostgreSQL |
| Dashboard, 15 pages | `tests/e2e/dashboard.test.mjs` |

## Meetings

| Feature | Evidence |
| --- | --- |
| Starting a meeting, joining, pre-join with camera preview and microphone level, device choice and switching, speaker test | `tests/e2e/meeting.test.mjs`, headless Chrome with fake devices |
| Grid and speaker view, active speaker, participant list, chat, reactions, hand raise | Same |
| Hosts and moderators: mute all, mute one, remove, lock, waiting room, end for everyone | Same, plus the security tests in `api/routes` |
| Recording from the meeting, checked with ffprobe | Same |
| Connection quality with real packet loss, recovery after a dropped connection, summary on leaving | Same |

## Recording, stream output and ingest

| Feature | Evidence |
| --- | --- |
| Room composite recording to a file, RTMP and RTMPS output, RTMP and WHIP ingest, URL pull | `tests/media`. Workers confined: no access to the worker's secrets, Landlock on ffmpeg, Chrome's sandbox on, read-only root in Compose |
| Recovery after a worker dies, leases, restarts of URL inputs, capacity limits | `tests/media`. Real RTMP services such as YouTube are untested, for want of accounts |

## SDKs and tools

| Feature | Evidence |
| --- | --- |
| TypeScript and React SDKs | Real Chrome on both servers, in rooms shared with the official client |
| Go, Node.js and Python server SDKs: tokens, webhooks, room client, REST egress and ingress clients | Real servers. Tokens work both ways with the official tools. Recording and RTMP with real workers. Untested on Node 20 and Python 3.10, the declared minimums. Not published |
| `streamkit` CLI | Built binary against real servers. The tests against the reference `lk` CLI have not run |

## Voice AI agents

| Feature | Evidence |
| --- | --- |
| Worker registration, automatic, Room API and token dispatch; agents do not hold rooms open | Both servers. The official worker runs unmodified on ours |
| Python framework: cascaded, turn-based realtime and full-duplex realtime pipelines, interruptions, tools, handoffs, model selection, observability | 121 of 121, with local Whisper, llama.cpp and Piper over real WebRTC |
| Node framework, same API shape | 44 of 45. One reference run made no tool call, cause unknown |
| Sealed provider credentials, readable only by workers | `api/routes` tests and a real control plane test |

Realtime pipelines have run only against a protocol fixture, and vendor adapters only against fixtures written from their
documentation, because there are no provider keys. See [VOICE_AI_PROVIDER_MATRIX.md](../VOICE_AI_PROVIDER_MATRIX.md).

## SIP

| Feature | Evidence |
| --- | --- |
| Separate `streamkit-sip` service: trunks, direct, caller and callee dispatch rules, inbound and outbound calls, G.711, DTMF both ways, REFER transfer, SIP over UDP, TCP and TLS, SRTP with SDES | Against pjsua and the official clients: `tests/sip` api 38 of 38, phones 46 of 46, abuse 21 of 21, calls 40 of 42 with both failures on the reference |
| Twirp SIP API for the official SDKs and CLI | Both servers |

No real trunk, carrier or NAT has been tested, and the service's image has never been built.
