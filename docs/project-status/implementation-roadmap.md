# Implementation roadmap

The order follows the phases in the project brief. Each phase ends with tests, and the project stays runnable throughout.
Status as of 2026-11-06. On that day the owner decided to finish what exists and build nothing new, so phases with work
left stop where they are.

| Phase | Scope | State |
| --- | --- | --- |
| 0. Audit | Verify everything, inventory what is missing | Done |
| 1. Finish existing work | SDKs, dashboard, recording, ingress, egress, meeting app, deployment, security tests | Done, with the gaps in `incomplete-features.md`. The meeting app has the whole list from the brief |
| 2. Media server gaps | `/rtc/v1`, SVC, A/V sync, data tracks, netem tests, Firefox | Done except SVC, which fails 2 of 16, and the thin-link cases in `incomplete-features.md` |
| 3. Voice AI | Agent server, dispatch, adapters, pipelines, turn detection, interruptions, tools, observability | Done with local engines. Vendor adapters wait on keys |
| 4. AI and avatars | Multimodal, avatar providers, custom engine, playground | Avatar interface only. Not continued |
| 5. SIP and E2EE | SIP service, trunks, calls, encryption, key management | SIP done on loopback. E2EE not built |
| 6. SDK ecosystem | iOS, Android, Flutter, React Native, Unity, server SDKs, CLI | Server SDKs and CLI done. Native SDKs not built |
| 7. Control plane | Organizations, members, secrets, server registration, deployment wizard, agent deployment, 50-page dashboard | Not continued beyond what phase 1 built |
| 8. Validation | Security audit, multi-region, load, browsers, final reports | The meeting security review's findings are fixed. The rest waits on real hardware, a Mac and an external audit |

Every requirement ends in one of three states: implemented and tested; unsupported, with a technical reason; or waiting on
credentials, licensing or infrastructure, with what is needed named.

## React Native SDK

The plan below was not carried out: it stopped with the decision of 2026-11-06.

Package `sdk-react-native/`, published as `@elnova-streamkit/react-native` after the rename. It goes ahead of the other native
SDKs because it reuses `sdk-js` and `sdk-react`, so it adds no new protocol stack. It starts once the `sdk-js` work is committed,
because it has to change `sdk-js`.

The official package has the same shape, a layer over its JS client. We use our own client instead.

| Part | Work | Proof |
| --- | --- | --- |
| `sdk-js` changes | Guard `document`, `window` and `<video>` attach, which React Native does not have | Browser SDK suite still passes on both servers |
| Globals | Register native WebRTC, and polyfill what Hermes lacks | Unit tests, Metro bundle |
| Views | `VideoTrack` on `RTCView` | Emulator |
| Hooks | Reuse `sdk-react`, with React Native versions of anything that touches the DOM | Unit tests, emulator |
| Audio session | Native module: Kotlin on Android, Swift on iOS | Android on the emulator. iOS needs a Mac |
| Example app | Join, publish camera and microphone, send data, subscribe, reconnect | Emulator, against both servers |

The native WebRTC module is either `@livekit/react-native-webrtc` 150.1.1, the fork the official SDK requires, or upstream
`react-native-webrtc` 155.0.1. Both are MIT. What the fork adds has not been read yet, and that decides it. All imports go through
one file, so the choice can change later. Expo support, through a config plugin and development builds, comes after that.

**Done means** a real call on an emulator, with the server seeing media arrive and the app decoding frames. A clean typecheck or
bundle is not evidence. iOS stays marked unverified until it runs on a Mac.

**Needs from the user:** a yes to Google's Android SDK license, to install the SDK and run the emulator here.

## Names

The project was renamed from REA Streaming to Elnova StreamKit on 2026-11-04.

| Thing | Name |
| --- | --- |
| Directory, Go module | `elnova-streamkit/`, `elnova-streamkit.local` |
| npm packages | `@elnova-streamkit/*` |
| Binaries | `streamkit-server`, `streamkit-egress`, `streamkit-ingress` |
| CLI | `streamkit` |
| Agents directory | `streamkit-agents/` |
| Environment variables | `STREAMKIT_*` |
| Metrics, cookie, Redis keys | `streamkit_*`, `streamkit_session`, `streamkit:*` |

"REA" remains only as the name of the reverse-engineering tool used during development. The CLI is `streamkit` because that tool's own CLI is `rea` and its
npm package is `rea-agents`.
