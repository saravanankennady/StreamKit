# Implementation audit

## Regression on 2026-11-06, with every branch merged

`develop` at `bec78ba` had every branch merged: server SDKs and CLI, meetings, agents, SIP, transport, the network suite, media
hardening and the meeting security fixes. Each suite ran once, one at a time, on one machine over loopback. Three faults between
branches showed up, and were fixed in `aecbc82` and `1a4c80b`; what failed was then run again at `1a4c80b`.

| Suite | Result | Commit |
| --- | --- | --- |
| Go tests, real PostgreSQL and Redis: server, API, database, SIP, egress, ingress, load | All packages pass | `1a4c80b`. At `bec78ba` two failed, both faults between branches |
| Conformance, official `livekit-client` in Chrome, this server | 41 of 41 | `bec78ba` |
| SDK interoperability | 78 of 78 | `bec78ba` |
| End to end, dashboard and meetings, with real workers | 121 of 121 | `1a4c80b`. At `bec78ba` 116 of 121: four recording tests, and one meeting test that waited on the host panel |
| Media services | 90 of 91 | `bec78ba`. The failure is below |

Faults the merges caused, now fixed:

- Media workers run the reaper under a database role that cannot reach SIP calls, which the SIP branch had added to it, so every
  pass failed for them. SIP calls are now reaped by the server alone.
- The network branch's check that a node address leaves no loopback socket no longer fitted the media branch's single socket for
  every address. The test now checks what matters: no loopback candidate goes out under the node's address.
- Chrome opens the recorder's launcher by a `file://` address built from the work directory, so a relative `work_dir` made it a
  host name and no recording started. The work directory is now always absolute.

Still failing: `a recorder cannot be sent to an address a project chose` in `tests/media/failure.test.mjs`. The recorder made no
connection to the project's ICE server, which is what the test is for, but the control step after it also failed: an ordinary
participant did not use that server either, so the test proves nothing yet. It had never run before this regression, and it
failed the same way when run on its own at `1a4c80b`. Not investigated.

Not run in this regression: `tests/sip`, `tests/agents`, `tests/network`, `tests/interoperability/transport`, `tests/cluster` with
Chrome, the compose suite, and the reference server's side of conformance and SDK tests. Their last results are on their branches,
in [completed-features.md](completed-features.md) and [incomplete-features.md](incomplete-features.md).

## Audit on 2026-11-03

Each claim about the project was checked against the code, not taken from earlier status reports. Audit of
Elnova StreamKit (formerly REA Streaming) at commit `50a8c1a`. Kept for the record: the section above supersedes it.

**Method.** Committed code was built and tested from a clean checkout, so uncommitted work could not affect the result.
Conformance tests run the official `livekit-client` 2.23 in real Chrome against both this server and the reference
server v1.13.11. A test only counts if the reference also passes it.

### Results

| Suite | Result | Notes |
| --- | --- | --- |
| Go tests, with the race detector, real PostgreSQL and Redis | 169 of 169 at `08cb78c` | Then 7 new test functions in `server/transport` |
| Conformance, this server | 39 of 39, 0 server warnings | |
| Conformance, reference server | 39 of 39 | |
| New resume regression test | Passes on both servers | Run separately, after the other 39 |
| Multi-node | 3 of 4, then 4 of 4 after a fix, three runs in a row | |
| `go vet`, `gofmt` | Clean | |

### A bug the audit found

The status said the media server was complete. The audit found a real bug in it: when a client's resume arrived while a
subscriber offer was unanswered, the ICE restart and any queued tracks were dropped, and the client stayed connected but
received no video. It happened in about 1 run in 8, against 0 of 8 on the reference. Earlier, the same failure had been
written off as a test race. It is fixed in `50a8c1a`, with regression tests. After the fix, 0 of 16 runs stalled. Sixteen
clean runs alone would be weak evidence; the deterministic test that failed before the fix is the stronger proof.

### Reported status against what the code shows

| Area | Reported | Verified |
| --- | --- | --- |
| Research | Completed; agents ongoing | Transport, control plane, SDK, media and agents analyses exist. SIP, E2EE, SDK ecosystem, CLI, Cloud, `/rtc/v1`, data tracks, SVC and A/V sync are being analyzed now |
| Media server | Completed | **Works, but "completed" overstates it.** Tested on Chrome and localhost only. Gaps below |
| Rooms, participants, tracks | Completed | Confirmed by conformance tests on both servers |
| APIs | Completed | **Partly.** Room API has 11 of 14 methods: `PerformRpc`, `ForwardParticipant` and `MoveParticipant` return `unimplemented`. Agent dispatch, SIP, egress and ingress Twirp services are absent. REST covers the core groups |
| Backend | Completed | **For the first brief.** Accounts, projects, keys and webhooks are tested. The new brief adds invitations, roles, quotas, project members, per-key limits and usage, and deployment management, none of which exist |
| Multi-server | Same-machine | Confirmed: two processes on one machine. Not run across machines |
| Security | Partial | Confirmed partial. Isolation, SSRF, CSRF, rate limits and token rejection are tested. No dependency or container scans, no XSS or SQL injection tests, signalling has no rate limit, no external audit |
| Deployment, monitoring | Partial | Confirmed partial. The server image builds and runs. nginx, TLS, coturn, Grafana, Helm and OTLP export have never been run |
| JS and React SDK | In progress | Not audited: uncommitted, and its agent has not reported |
| Dashboard | In progress | Not audited: uncommitted, and its agent has not reported. 15 of the 50 pages in the new brief |
| Recording, ingress, egress | In progress | Not audited: uncommitted, and its agent has not reported |
| Voice AI | Research only | Confirmed. Only the token grant and `AGENT` participant kind exist |
| Meeting app, load test, parity report | Not started | Confirmed, except that the load generator exists and has only run at 5 clients |

### Known media server gaps at the time

- `/rtc/v1`: not implemented, and clients fall back to `/rtc`
- SVC: all layers forwarded, none selected
- A/V sync: sender reports use arrival time at the server, not the publisher's clock
- Data tracks: unsupported
- NACK and congestion: measured by hand with `tc netem` on one machine (2026-11-04), with no automated suite yet and no real network
- Browsers: Chrome only

## What this environment cannot verify

| Needs | For |
| --- | --- |
| macOS and Xcode | iOS SDK, Safari |
| Several physical machines, real networks, a domain | Multi-machine, multi-region, NAT, TLS |
| AI provider API keys | Real STT, TTS and non-Ollama LLM tests |
| A SIP trunk | Real phone calls |
| An independent security firm | External audit |
| Sudo | WebKit system libraries, unless Docker is enough |

Available here: Docker, `tc netem` inside containers (confirmed working), Firefox through Playwright, Ollama with no GPU, and
KVM (`/dev/kvm` is readable and writable by this account). KVM means an Android emulator should run here without sudo, but none
has been run yet, and the Android SDK is not installed. Installing it means accepting Google's Android SDK license, which is the
user's decision.
