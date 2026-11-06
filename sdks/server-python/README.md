# Elnova StreamKit server SDK for Python

`elnova-streamkit-server`, imported as `elnova_streamkit`: access tokens, webhook checks, and clients for rooms, recording and
ingest. Run it on your server: it needs your API secret, which must never reach a browser or an app.

## Install

It is not on PyPI yet, so install it from this repository. Python 3.10 or later.

```bash
pip install /path/to/elnova-streamkit/sdks/server-python
```

## Example

```python
import asyncio
from datetime import timedelta

from elnova_streamkit import AccessToken, StreamKitAPI, VideoGrants
from elnova_streamkit.proto.livekit_room_pb2 import CreateRoomRequest, ListParticipantsRequest, SendDataRequest

URL, KEY, SECRET = "ws://127.0.0.1:7880", "devkey", "secret"  # what bin/streamkit-server --dev uses


async def main() -> None:
    # A token for someone to join with. The client SDKs take this.
    token = (
        AccessToken(KEY, SECRET)
        .with_identity("alice")
        .with_name("Alice")
        .with_ttl(timedelta(hours=1))
        .with_grants(VideoGrants(room_join=True, room="lobby"))
        .to_jwt()
    )
    print("token:", token)

    # The room API.
    async with StreamKitAPI(URL, KEY, SECRET) as api:
        room = await api.room.create_room(CreateRoomRequest(name="lobby", empty_timeout=300))
        print("created", room.name, room.sid)
        listed = await api.room.list_participants(ListParticipantsRequest(room="lobby"))
        print(len(listed.participants), "people in the room")
        await api.room.send_data(SendDataRequest(room="lobby", data=b"hello", topic="greetings"))


asyncio.run(main())
```

The server and credentials can also come from `STREAMKIT_URL`, `STREAMKIT_API_KEY` and `STREAMKIT_API_SECRET`.

A webhook handler. Pass the body as it arrived, because the signature covers the exact bytes:

```python
from elnova_streamkit import TokenVerifier, WebhookReceiver

receiver = WebhookReceiver(TokenVerifier(KEY, SECRET))


@app.post("/webhook")
async def webhook(request):
    try:
        event = receiver.receive(await request.body(), request.headers.get("Authorization", ""))
    except Exception:
        return Response(status_code=401)
    print(event.event, event.room.name)
    return Response()
```

A recording, which needs a server with a control plane and a worker:

```python
job = await api.egress.start_room_composite_egress("lobby", format="mp4")
# later:
await api.egress.stop_egress(job.id)
# When get_egress says it has ended and its status is complete, the file is ready:
with open("recording.mp4", "wb") as f:
    await api.egress.download_recording(job.id, f)
```

A failure the server reports is a `ServerError`, with `code` (such as `not_found`), `status`, and `fields` for validation
failures. `NotSupportedError` says the server cannot do this at all. The wire types are in `elnova_streamkit.proto`, named as
the protocol names them. They live in a descriptor pool of their own, so they can be imported beside the official `livekit`
packages in one process, and the tests do.

## What is here

| | |
| --- | --- |
| Tokens | `AccessToken` with video, SIP, agent, inference, observability, agent endpoint and agent thread grants, room configuration, preset, kind and attributes. `TokenVerifier` makes the same checks a server does: HS256 only, the right issuer, an expiry, 60 seconds of leeway |
| Webhooks | `WebhookReceiver` checks the signature and the SHA-256 of the exact body. `sign_webhook`, for testing a receiver |
| Rooms | `api.room`, over Twirp in protobuf: every method of the service |
| Recording | `api.egress`, over this server's REST API: room composite, participant, audio only and video only, to a file, RTMP or both. List, get, stop, download, retry, delete |
| Ingest | `api.ingress`, over this server's REST API: RTMP, WHIP and URL. Create, list, get, update, stop, rotate key, logs, delete |
| Agent dispatch | `api.agent_dispatch`, over Twirp. Works against the reference server. This server does not have the service yet: see below |

The room client and tokens work against this server and against the reference server, and the tests run on both, beside the
official Python SDK as a comparison.

## What is not supported

**This server does not implement these:**

- `perform_rpc`, `forward_participant`, `move_participant`: a `ServerError` with the code `unimplemented`. The open-source
  reference server does not implement forward and move either.
- `api.agent_dispatch`: `NotSupportedError`, because the server has no such service.
- `start_web_egress`, `start_track_composite_egress`, `start_track_egress`, `update_layout`, `update_stream`:
  `NotSupportedError`, with no call to the server. It has no such recording types, and cannot change a running job. Use
  `start_audio_only_egress` or `start_video_only_egress` for one kind of media.

**Not included:**

- SIP. Tokens can carry `SIPGrants`, because the protocol has them, but there is no client: this server has no SIP service.
- Egress and ingress against the reference server's Twirp services. These clients speak this server's REST API, so against
  anything else they raise `NotSupportedError`. In the other direction, official SDKs' egress and ingress clients cannot talk
  to this server, because it has no Twirp service for them.
- Filters on `list_egress` and `list_ingress`. The API lists everything: `list_egress` in pages, and the rest is up to you.
- Cloud services: connectors, phone numbers, hosted agents. Region failover, which only applies to Cloud hostnames.
- A real-time room client. To join a room from Python as a participant, use the official `livekit` package.

**Grants this server does not act on:** SIP, agent, inference, observability, agent endpoint and agent thread. Tokens can
carry them and it ignores them. A token's room configuration applies only its timeouts, participant limit and metadata, and
`room_preset` is ignored.

## Testing

Use a virtual environment. These commands run from this directory.

```bash
pip install -e '.[test]'
pytest                                   # needs nothing running: tokens and webhooks only

# Against real servers: this server and the reference server, with real participants. Needs Go on PATH.
STREAMKIT_TEST_DATABASE_URL='postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable' pytest

# And recording and ingest, with real workers. Needs Chrome and ffmpeg.
STREAMKIT_TEST_WORKERS=1 STREAMKIT_TEST_DATABASE_URL='...' pytest
```

The tests start everything themselves, on free ports, with a database of their own: see `tests/sdk/fixture`. They find the
reference server at `reference/oracle/bin/x/livekit-server`, beside or above the repository, and skip its half if it is not
there. `livekit-api` is a test dependency for comparing against, and this SDK does not use it. All of this runs on one machine
over loopback, so it says nothing about real networks.

After changing the vendored `.proto` files, run `python scripts/generate_proto.py` with `grpcio-tools` installed.
