# Elnova StreamKit server SDK for Go

Access tokens, webhook checks, and clients for rooms, recording and ingest. Run it on your server: it needs your API
secret, which must never reach a browser or an app.

## Install

The package is `elnova-streamkit.local/sdks/server-go`, in the same module as the rest of this repository. That module is
not published anywhere yet, so point a `replace` at a checkout:

```bash
go mod edit -require=elnova-streamkit.local@v0.0.0 -replace=elnova-streamkit.local=/path/to/elnova-streamkit
go mod tidy
```

Go only builds what you import, so this pulls in protobuf and a JWT library, and not the server.

## Example

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
)

func main() {
	url, key, secret := "ws://127.0.0.1:7880", "devkey", "secret" // what bin/streamkit-server --dev uses
	ctx := context.Background()

	// A token for someone to join with. The client SDKs take this.
	token, err := streamkit.NewAccessToken(key, secret).
		SetIdentity("alice").SetName("Alice").SetValidFor(time.Hour).
		SetVideoGrant(&streamkit.VideoGrant{RoomJoin: true, Room: "lobby"}).
		ToJWT()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "token:", token)

	// The room API.
	client := streamkit.NewClient(url, key, secret)
	room, err := client.Room.CreateRoom(ctx, &pb.CreateRoomRequest{Name: "lobby", EmptyTimeout: 300})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("created", room.GetName(), room.GetSid())

	participants, err := client.Room.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: "lobby"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(participants.GetParticipants()), "people in the room")
}
```

A webhook handler:

```go
receiver := streamkit.NewWebhookReceiver(streamkit.Keys{apiKey: apiSecret})

http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
	event, err := receiver.ReceiveRequest(r)
	if err != nil {
		http.Error(w, "not from our server", http.StatusUnauthorized)
		return
	}
	log.Println(event.GetEvent(), event.GetRoom().GetName())
})
```

A recording, which needs a server with a control plane and a worker:

```go
job, err := client.Egress.StartRoomCompositeEgress(ctx, "lobby", streamkit.EgressOutput{Format: "mp4"})
// later:
job, err = client.Egress.StopEgress(ctx, job.ID)
job, err = client.Egress.WaitForEgress(ctx, job.ID, (*streamkit.Egress).Ended)
_, err = client.Egress.DownloadRecording(ctx, job.ID, file)
```

`streamkit.ErrorCode(err)` gives a server error's code, such as `not_found`, and `errors.Is(err, streamkit.ErrNotSupported)`
says the server cannot do this at all.

## What is here

| | |
| --- | --- |
| Tokens | `NewAccessToken` with video, SIP, agent, inference, observability, agent endpoint and agent thread grants, room configuration, preset, kind and attributes. `VerifyToken` makes the same checks a server does: HS256 only, a known issuer, an expiry, 60 seconds of leeway |
| Webhooks | `WebhookReceiver` checks the signature and the SHA-256 of the exact body. `SignWebhook`, for testing a receiver |
| Rooms | `RoomClient`, over Twirp in protobuf: every method of the service |
| Recording | `EgressClient`, over this server's REST API: room composite, participant, audio only and video only, to a file, RTMP or both. List, get, stop, download, retry, delete |
| Ingest | `IngressClient`, over this server's REST API: RTMP, WHIP and URL. Create, list, get, update, stop, rotate key, logs, delete |
| Agent dispatch | `AgentDispatchClient`, over Twirp. Works against the reference server. This server does not have the service yet: see below |

The room client and tokens work against this server and against the reference server, and the tests run on both.

## What is not supported

**This server does not implement these, so they return an error that wraps `ErrNotSupported`:**

- `PerformRpc`: the server answers `unimplemented`.
- `ForwardParticipant` and `MoveParticipant`: the server answers `unimplemented`. The open-source reference server does not
  implement them either.
- `AgentDispatchClient`: the server has no such service.
- `StartWebEgress`, `StartTrackCompositeEgress`, `StartTrackEgress`, `UpdateLayout`, `UpdateStream`: the server has no such
  recording types and cannot change a running job. These never call the server. Use `StartAudioOnlyEgress` or
  `StartVideoOnlyEgress` for one kind of media.

**Not included:**

- SIP. There is a `SIPGrant` for tokens, because the protocol has it, but no client: this server has no SIP service, and the
  SIP definitions are not vendored here. Tokens carry the grant, and this server ignores it.
- Egress and ingress against the reference server's Twirp services. These clients speak this server's REST API, so against
  anything else they return `ErrNotSupported`. In the other direction, official SDKs' egress and ingress clients cannot talk
  to this server, because it has no Twirp service for them.
- Filters on `ListEgress` and `ListIngress`. The API lists everything: `ListEgress` in pages, and the rest is up to you.
- Cloud services: connectors, phone numbers, hosted agents, simulations.
- A real-time room client. To join a room from Go as a participant, see `tests/load/loadgen`, which is a test client, or use
  an official client SDK.

**Grants this server does not act on:** SIP, agent, inference, observability, agent endpoint and agent thread. Tokens can
carry them and it ignores them. A token's room configuration applies only its timeouts, participant limit and metadata, and
`roomPreset` is ignored.

## Testing

```bash
go test ./sdks/server-go                 # needs nothing running: tokens and webhooks only

# Against real servers: this server and the reference server, with real participants.
STREAMKIT_TEST_DATABASE_URL='postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable' \
  go test ./sdks/server-go

# And recording and ingest, with real workers. Needs Chrome and ffmpeg.
STREAMKIT_TEST_WORKERS=1 STREAMKIT_TEST_DATABASE_URL='...' go test ./sdks/server-go
```

The tests start everything themselves, on free ports, with a database of their own: see `tests/sdk/fixture`. They find the
reference server at `reference/oracle/bin/x/livekit-server`, beside or above the repository, and skip its half if it is not
there. All of this runs on one machine over loopback, so it says nothing about real networks.
