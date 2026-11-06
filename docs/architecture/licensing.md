# Licensing and independence

Written for: whoever signs off on what ships. This is engineering's reading of the licenses, not legal
advice. Have counsel review before distributing the product.

## Reference repositories

| Repository | License | How we use it |
| --- | --- | --- |
| livekit/livekit | Apache-2.0 | Read as a behavioural reference. No code copied. |
| livekit/protocol | Apache-2.0 | `.proto` files vendored unmodified in `proto/vendor/livekit/` for wire compatibility. The tables that test trunk and dispatch rules are copied into `sip/rules/reference_table_test.go`, with the notice kept and the changes stated. |
| livekit/client-sdk-js | Apache-2.0 | Read as a reference. The npm package is a **test-only** dependency. |
| livekit/components-js | Apache-2.0 | Read as a reference. |
| livekit/egress, livekit/ingress, livekit/livekit-cli | Apache-2.0 | Read as references. |
| livekit/sip | Apache-2.0 | Read as a behavioural reference. No code copied. Built separately, and run only to compare behaviour in tests. |
| livekit/server-sdk-go, livekit/node-sdks, livekit/python-sdks | Apache-2.0 | Read as references for the API shape of `sdks/`. The npm `livekit-server-sdk` and Python `livekit-api` packages are **test-only** dependencies. |
| morluto/rea | MIT | Used as a tool. Not a dependency of the product. |

Apache-2.0 would permit reusing LiveKit's code with attribution. We don't, because the goal is
infrastructure we own and can evolve independently.

## What is reused and what is independent

**Reused under license**

- The wire protocol definitions, unmodified, with their `LICENSE` and `NOTICE` kept beside them. Using the
  same field numbers is what lets LiveKit protocol clients talk to this server. The generated code in
  `pkg/pb/`, `sdk-js/src/proto/`, `sdks/server-node/src/proto/` and `sdks/server-python/elnova_streamkit/proto/`
  derives from these files. The two server SDKs that ship alone carry their own notice and a copy of the license.
- Open-source libraries under their own licenses: Pion (MIT), gorilla/websocket (BSD-2-Clause),
  golang-jwt (MIT), Prometheus client (Apache-2.0), protobuf-go (BSD-3-Clause), emiago/sipgo (BSD-2-Clause),
  thesyncim/gopus (BSD-3-Clause), icholy/digest (MIT). Their full list is in
  `go.mod`, and a license audit should run before distribution.

**Independently implemented**

Everything else: signalling, rooms, participants, track management, forwarding, RTP rewriting, layer
selection, bandwidth allocation, and the test suite. Written from observed behaviour. The analysis
notes behind it described behaviour in their own words, pasted no source, and are not part of this repository.

## Agents

`streamkit-agents/` is independently implemented on the same terms. It uses the wire protocol above, and no code from `livekit/agents`.
LiveKit's rtc SDK and agents framework are used **only as test oracles**, in `tests/agents/oracle`, never in the product. Its dependencies
are listed in `streamkit-agents/README.md`.

`MODEL_LICENSE` in livekit/agents forbids using LiveKit's models with any other framework. No LiveKit model is downloaded or used
anywhere. The oracle test does not install `livekit-local-inference`, which is where those models are: a stand-in meets the import.

The engines used to prove the framework are not part of the product. One of them, Piper, is GPL-3.0, and it runs as a separate process
for that reason. See `streamkit-agents/local-engines/README.md`.

## Trademarks

"LiveKit" appears only to describe compatibility and to name the vendored protocol package, whose
`package livekit` is part of the wire format's schema names. The product doesn't use LiveKit's logos or
imply affiliation; `NOTICE.md` says so explicitly.

Our default wire-reported server version (`1.9.0`) is a compatibility level, chosen because clients gate
features on it. It doesn't claim to be a LiveKit release.

## Decisions for the owner

1. **The product's own license.** `LICENSE` currently says all rights reserved, with no license granted. It
   is a placeholder that keeps your options open, because granting a license is the hard direction to reverse.
2. **The name "Elnova StreamKit".** No trademark search has been done. Do one before any public release, because
   "StreamKit" is a common word in this field.
