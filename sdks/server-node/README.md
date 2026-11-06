# Elnova StreamKit server SDK for Node.js

`@elnova-streamkit/server-sdk`: access tokens, webhook checks, and clients for rooms, recording and ingest, in TypeScript.
Run it on your server: it needs your API secret, which must never reach a browser. `AccessToken` throws if it finds itself in
a page.

## Install

It is not on npm yet, so build it from this repository and depend on the folder. Node 20 or later.

```bash
cd sdks/server-node && npm ci && npm run build
cd /your/app && npm install /path/to/elnova-streamkit/sdks/server-node
```

## Example

```ts
import { AccessToken, DataPacket_Kind, StreamKitAPI } from '@elnova-streamkit/server-sdk';

const url = 'ws://127.0.0.1:7880';
const [key, secret] = ['devkey', 'secret']; // what bin/streamkit-server --dev uses

// A token for someone to join with. The client SDKs take this.
const token = new AccessToken(key, secret, { identity: 'alice', name: 'Alice', ttl: '1h' });
token.addGrant({ roomJoin: true, room: 'lobby' });
console.error('token:', await token.toJwt());

// The room API.
const api = new StreamKitAPI(url, key, secret);
const room = await api.room.createRoom({ name: 'lobby', emptyTimeout: 300 });
console.log('created', room.name, room.sid);
console.log((await api.room.listParticipants('lobby')).length, 'people in the room');
await api.room.sendData('lobby', new TextEncoder().encode('hello'), DataPacket_Kind.RELIABLE, { topic: 'greetings' });
```

The server and credentials can also come from `STREAMKIT_URL`, `STREAMKIT_API_KEY` and `STREAMKIT_API_SECRET`.

A token can say how the room it creates should be set up. The wire types use `@bufbuild/protobuf`:

```ts
import { create } from '@bufbuild/protobuf';
import { RoomConfigurationSchema } from '@elnova-streamkit/server-sdk';

token.roomConfig = create(RoomConfigurationSchema, { emptyTimeout: 300, maxParticipants: 10 });
```

A webhook handler. Pass the body as it arrived, because the signature covers the exact bytes:

```ts
import { WebhookReceiver } from '@elnova-streamkit/server-sdk';

const receiver = new WebhookReceiver(key, secret);

app.post('/webhook', express.raw({ type: 'application/webhook+json' }), async (req, res) => {
  try {
    const event = await receiver.receive(req.body, req.get('Authorization'));
    console.log(event.event, event.room?.name);
    res.sendStatus(200);
  } catch {
    res.sendStatus(401);
  }
});
```

A recording, which needs a server with a control plane and a worker:

```ts
const job = await api.egress.startRoomCompositeEgress('lobby', { format: 'mp4' });
// later:
await api.egress.stopEgress(job.id);
// When getEgress says its status is complete, the file is ready:
const file = await api.egress.downloadRecording(job.id);
```

A failure the server reports is a `ServerError`, with `code` (such as `not_found`), `status`, and `fields` for validation
failures. `NotSupportedError` says the server cannot do this at all.

## What is here

| | |
| --- | --- |
| Tokens | `AccessToken` with video, SIP, agent, inference, observability, agent endpoint and agent thread grants, room configuration, preset, kind and attributes. `TokenVerifier` makes the same checks a server does: HS256 only, the right issuer, an expiry, 60 seconds of leeway |
| Webhooks | `WebhookReceiver` checks the signature and the SHA-256 of the exact body. `signWebhook`, for testing a receiver |
| Rooms | `RoomServiceClient`, over Twirp in JSON: every method of the service |
| Recording | `EgressClient`, over this server's REST API: room composite, participant, audio only and video only, to a file, RTMP or both. List, get, stop, download, retry, delete |
| Ingest | `IngressClient`, over this server's REST API: RTMP, WHIP and URL. Create, list, get, update, stop, rotate key, logs, delete |
| Agent dispatch | `AgentDispatchClient`, over Twirp. Works against the reference server. This server does not have the service yet: see below |

The room client and tokens work against this server and against the reference server, and the tests run on both, beside the
official Node SDK as a comparison.

## What is not supported

**This server does not implement these:**

- `performRpc`, `forwardParticipant`, `moveParticipant`: a `ServerError` with the code `unimplemented`. The open-source
  reference server does not implement forward and move either.
- `AgentDispatchClient`: `NotSupportedError`, because the server has no such service.
- `startWebEgress`, `startTrackCompositeEgress`, `startTrackEgress`, `updateLayout`, `updateStream`: `NotSupportedError`, with
  no call to the server. It has no such recording types, and cannot change a running job. Use `startAudioOnlyEgress` or
  `startVideoOnlyEgress` for one kind of media.

**Not included:**

- SIP. Tokens can carry a `SIPGrant`, because the protocol has it, but there is no client: this server has no SIP service.
- Egress and ingress against the reference server's Twirp services. These clients speak this server's REST API, so against
  anything else they throw `NotSupportedError`. In the other direction, official SDKs' egress and ingress clients cannot talk
  to this server, because it has no Twirp service for them.
- Filters on `listEgress` and `listIngress`. The API lists everything: `listEgress` in pages, and the rest is up to you.
- Cloud services: connectors, phone numbers, hosted agents. Region failover, which only applies to Cloud hostnames.
- A real-time room client. Use `@elnova-streamkit/client` in a browser.

**Grants this server does not act on:** SIP, agent, inference, observability, agent endpoint and agent thread. Tokens can
carry them and it ignores them. A token's room configuration applies only its timeouts, participant limit and metadata, and
`roomPreset` is ignored.

## Testing

```bash
npm test                                 # needs nothing running: tokens and webhooks only

# Against real servers: this server and the reference server, with real participants. Needs Go on PATH.
STREAMKIT_TEST_DATABASE_URL='postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable' npm test

# And recording and ingest, with real workers. Needs Chrome and ffmpeg.
STREAMKIT_TEST_WORKERS=1 STREAMKIT_TEST_DATABASE_URL='...' npm test
```

The tests start everything themselves, on free ports, with a database of their own: see `tests/sdk/fixture`. They find the
reference server at `reference/oracle/bin/x/livekit-server`, beside or above the repository, and skip its half if it is not
there. `livekit-server-sdk` is a dev dependency for comparing against, and this SDK does not use it. All of this runs on one
machine over loopback, so it says nothing about real networks.
