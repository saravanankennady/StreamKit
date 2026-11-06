# Third-party notices

Elnova StreamKit is an independent project. It is not affiliated with, endorsed by, or sponsored by
LiveKit, Inc. "LiveKit" is used only to describe protocol compatibility.

## LiveKit protocol definitions

`proto/vendor/livekit/` contains unmodified `.proto` files from
[livekit/protocol](https://github.com/livekit/protocol), licensed under the Apache License 2.0. See
that directory's `LICENSE`, `NOTICE` and `README.md`. The Go code in `pkg/pb/`, and the TypeScript in `sdk-js/src/proto/` and
`sdks/server-node/src/proto/`, and the Python in `sdks/server-python/elnova_streamkit/proto/`, are generated from them. The
Node and Python server SDKs carry their own `NOTICE.md` and a copy of the license, because they ship apart from this file.

## API shape of the server SDKs

`sdks/` and `cli/` follow the names and shapes of [livekit/server-sdk-go](https://github.com/livekit/server-sdk-go),
[livekit/node-sdks](https://github.com/livekit/node-sdks), [livekit/python-sdks](https://github.com/livekit/python-sdks) and
[livekit/livekit-cli](https://github.com/livekit/livekit-cli), all Apache-2.0, so that they are familiar to anyone who knows
those. No code was copied from them.

## SIP

`sip/rules/reference_table_test.go` copies test tables from [livekit/protocol](https://github.com/livekit/protocol)
(`sip/sip_test.go`, commit `eecdef3`), under the Apache License 2.0, keeping their copyright notice and saying what changed. They are
how the dispatch and trunk rules in `sip/rules` are checked against the reference. `proto/vendor/livekit/livekit_sip.proto` is
vendored unmodified with the other protocol definitions.

`sip/` otherwise follows the behaviour of [livekit/sip](https://github.com/livekit/sip) (Apache-2.0), and copies no code from
it. Names that integrations depend on, such as the `sip.*` participant
attributes, the `sip.callStatus` values and the `lk.sip.*` RPC methods, are the same on purpose.

The service depends on these under their own licenses: [emiago/sipgo](https://github.com/emiago/sipgo) (BSD-2-Clause) for SIP,
[thesyncim/gopus](https://github.com/thesyncim/gopus) (BSD-3-Clause) for Opus, [icholy/digest](https://github.com/icholy/digest) (MIT) for digest parsing, and Pion (MIT) for RTP, SRTP, SDP and WebRTC.
Why each was chosen is in `docs/deployment/sip.md`.

## Docker's seccomp profile

`infrastructure/docker/chrome-seccomp.json` is Docker's default seccomp profile from
[moby/profiles](https://github.com/moby/profiles), licensed under the Apache License 2.0, with one rule added at the end.

## Test-only dependencies

`tests/interoperability/` and `tests/sip/` use the official `livekit-client` npm package (Apache-2.0) as a black-box
test client, and `tests/sip/` the official `livekit-server-sdk` and, as an independent SIP phone, pjsua from
[pjproject](https://github.com/pjsip/pjproject) (GPL-2.0). pjsua is built separately under `.toolchain/`, run as a separate process
and never linked with or shipped in anything here. `sdks/server-node/` and `sdks/server-python/` compare themselves against the official `livekit-server-sdk` npm
package and `livekit-api` Python package (both Apache-2.0) in their tests. None of these is ever part of a shipped Elnova
StreamKit artifact.

## Go and npm dependencies

Other dependencies are listed in `go.mod` and each `package.json`, under their own licenses.
