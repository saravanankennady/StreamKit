package streamkit_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
)

// The example in README.md. It is here so that it keeps compiling.
func Example() {
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

func ExampleWebhookReceiver() {
	receiver := streamkit.NewWebhookReceiver(streamkit.Keys{"devkey": "secret"})

	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		event, err := receiver.ReceiveRequest(r)
		if err != nil {
			http.Error(w, "not from our server", http.StatusUnauthorized)
			return
		}
		log.Println(event.GetEvent(), event.GetRoom().GetName())
	})
}

func ExampleEgressClient() {
	ctx := context.Background()
	client := streamkit.NewClient("ws://127.0.0.1:7880", "a-project-key", "its-secret")
	file, err := os.Create("recording.mp4")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	job, err := client.Egress.StartRoomCompositeEgress(ctx, "lobby", streamkit.EgressOutput{Format: "mp4"})
	if err != nil {
		log.Fatal(err)
	}
	// later:
	if job, err = client.Egress.StopEgress(ctx, job.ID); err != nil {
		log.Fatal(err)
	}
	if job, err = client.Egress.WaitForEgress(ctx, job.ID, (*streamkit.Egress).Ended); err != nil {
		log.Fatal(err)
	}
	if _, err = client.Egress.DownloadRecording(ctx, job.ID, file); err != nil {
		log.Fatal(err)
	}
}
