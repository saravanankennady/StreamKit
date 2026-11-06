# Implementation roadmap

Status of the milestones from the project brief. "Done" means the milestone's stated deliverable was
demonstrated and has automated tests. Feature-level status is in
[completed-features.md](../project-status/completed-features.md) and [incomplete-features.md](../project-status/incomplete-features.md).

| Milestone | Status | Deliverable | Evidence |
| --- | --- | --- | --- |
| 1. Research | Done | Technical architecture report | Kept outside this repository |
| 2. Foundation | Done | Runnable development environment | `docker compose up`, `streamkit-server --dev`, integration tests against real PostgreSQL and Redis |
| 3. Core SFU | Done | Two browsers exchange real audio and video on our server | `tests/interoperability/conformance.test.mjs` |
| 4. SDK | Not started | Reusable client SDK | |
| 5. Dashboard | Not started | Functional developer console | |
| 6. Meeting app | Not started | Functional conferencing application | |
| 7. Media services | Not started | Recording, egress, ingress | Analysis only |
| 8. Production readiness | Partial | Production deployment candidate | Clustering tested. Compose, Helm, monitoring written. Load ladder and real-network deployment not yet done |
| 9. Parity validation | Started | Parity and performance reports | The conformance suite runs on both servers. |
| 10. Voice AI agents | Analysis started | Multiple LLM, STT and TTS providers, prompt control, realtime avatars | Added after the first plan. See below |

## Order of work

The order follows dependencies. Each step leaves the system runnable.

1. **Room API.** Done: the Twirp-compatible `RoomService`, tested on both servers.
2. **Foundation.** Done: Docker Compose, PostgreSQL and migrations, and the control plane.
3. **SDK.** `sdk-js`, then `sdk-react`. The conformance suite gets a second client, so the same scenarios
   prove our SDK and server together.
4. **Dashboard** on real backend data, then the **meeting application** on `sdk-react`.
5. **Media services.** Recording and egress first, then ingress. Our own SFU can offer a
   simpler internal RTP tap than a full client participant.
6. **Production readiness.** Multi-node routing with Redis, monitoring, load tests, security review.

## Scope correction: voice AI agents

The first plan left out the voice AI side: an agents framework, provider plugins and avatar integrations. That was an
engineering decision made without asking, and it was wrong for the brief. It is now milestone 10, and it builds on the SDK
and media work.

## Architectural decisions so far

| Decision | Why | Alternatives considered |
| --- | --- | --- |
| Speak an existing open wire protocol, from its vendored Apache-2.0 protos (see `NOTICE.md`) | Lets the official client act as a test oracle, gives developers a familiar model, and the license permits it | A new protocol would have no oracle and no client ecosystem |
| Dual peer connection first (`/rtc`) | Simpler negotiation, and clients fall back to it | Single peer connection is on the roadmap |
| Stock Pion, not a fork | Independence and simpler upgrades. The only fork-only API the reference server uses is off by default | Using the fork would tie us to their release cycle |
| Judge a subscriber's capacity from receiver reports: loss, round trips and repeated keyframe requests (`server/sfu/capacity.go`). No limit until those show congestion | Pion's GCC fell from 10 Mbps to 0.16 Mbps in 15 s on a link with room to spare and 8 ms of jitter, which is normal WiFi. It also limits NACK retransmissions, so lost packets were not replaced | Pion GCC with TWCC, tried first and removed. The reference's REMB plus probing would need a custom prober |
| One Go module, one server binary | The brief says to avoid needless services. Media, signalling and the room API share state in-process | Separate binaries add operational cost with no present benefit |
| Rooms keyed by project from day one | Tenant isolation is hard to retrofit | |
