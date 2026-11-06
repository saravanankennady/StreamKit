# Incomplete features

Exists, but with known failures or parts that have not run. Features with no code at all are under
[Not built](#not-built).

| Feature | State | What is left |
| --- | --- | --- |
| SVC layer selection | `transport/svc.test.mjs` 14 of 16 | Two L3T3_KEY runs on our server: a subscriber could read no descriptors at the lowest layer. Likely a key frame requested before DTLS finishes is lost (`server/sfu/downtrack.go`, `Bind`). Not fixed |
| Thin and unsteady links | `tests/network`, 9 of 43 cases miss a check on ours | A 3.6 s gap at 20% loss; the jitter fix in 64299df not re-measured; one 172 s stall with 200 ms round trip on the sharer's link, unconfirmed; 7.8 fps after a network switch; screen share at 1 Mbps from the start at 0.9 fps against 7.4 on the reference; screen share at 500 kbps frozen on both servers. On a clean link the top layer arrives 3.5 to 4.6 s after the first frame, against 1.4 to 1.8 s on the reference |
| `/rtc/v1` beyond Chrome | Chrome passes | Firefox, Safari and native SDKs have not run on it, and a 200 from validate ends their fallback too |
| Media hardening | Fixed and tested: media suite 90 of 91 on `develop` | The test that a recorder cannot reach a project's ICE servers proves nothing yet: its control step fails too. The compose suite's last fix and the 30-minute drift check have not run. The workers still hold the master key, which needs a design decision |
| Migration `0005` | Works on one node | Two nodes starting together on a fresh database can deadlock: it builds an index concurrently while holding the migration lock |
| WHIP in a cluster | One node | A publish to a node that does not own the room is not routed to the owner |
| SIP | Tested on loopback | The `ccdc536` DTMF test change has not run. The image has never been built. No Helm entry. No SIP client in `sdks/` or `cli/` |
| Agents | Python 121 of 121, Node 44 of 45 | The Node failure is not root caused. STT and TTS fallback adapters; each job in its own process; a broker across nodes; dashboard pages for credentials and sessions |
| Room API | 11 of 14 methods | `PerformRpc`. Forward and Move are Cloud-only in the reference |
| Server SDKs and CLI | Merged and tested | Clients for what our server lacks. Their egress and ingress clients use REST, so they do not work against the reference. Publishing, because the Go module path cannot be fetched. The `lk` tests |
| Deployment | Compose runs | Helm has never run. No cluster or real servers here |
| Monitoring | Metrics and dashboard JSON written | Run Grafana and OTLP export for real. Centralized logging, Alertmanager |
| Security | Core protections and the review's findings tested | Dependency and container scans, an external audit |
| Load testing | Generator runs at 5 clients | The ladder to 500, on a quiet machine |

## Not built

On 2026-11-06 the owner decided to finish what exists and build nothing new, so these stay unbuilt until that changes.

| Area | Missing | Note |
| --- | --- | --- |
| End-to-end encryption | SDK encryption, key management, `sif_trailer` | |
| Native SDKs | Swift, Kotlin, Flutter, React Native, Unity, C++ | Swift and Safari need a Mac. Android needs the owner to accept the Android SDK license |
| Avatars | Vendor adapters and a custom engine. Only the interface exists | |
| Agent Playground and agent observability pages | The API exists, no pages | |
| Control plane | Invitations, member and role management, quotas, per-key limits, project secrets, server registration, deployment wizard, agent deployment | |
| Dashboard | About 35 of the brief's 50 pages | |
| Final reports | The brief's set of final documents, such as the performance benchmark report | Needs benchmarks on real hardware |
