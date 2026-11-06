package streamkit_test

import (
	"errors"
	"strings"
	"testing"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
	"elnova-streamkit.local/tests/sdk/fixture"
)

// outcome is what a call came to, for comparing two servers.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	var e *streamkit.Error
	if errors.As(err, &e) {
		return e.Code + ": " + e.Message
	}
	return "other: " + err.Error()
}

// The same mistakes should get the same answers from both servers, because code that uses the API branches on them.
// Where this server gives a different answer on purpose, the case says what each answers and why, so that the
// difference is recorded here, and a change on either side fails the test.
func TestMistakesGetTheSameAnswerOnBothServers(t *testing.T) {
	if fx == nil || fx.Info.Reference == nil {
		t.Skip("needs both servers")
	}
	ctx := testContext(t)
	long := strings.Repeat("x", 300)
	const (
		notFoundRoom        = "not_found: requested room does not exist"
		notFoundParticipant = "not_found: participant does not exist"
		// The reference sends these to whichever node has the room or participant, finds none after three seconds, and
		// answers with that. This server knows at once, and says what is missing.
		noResponse = "unavailable: no response from servers"
	)
	cases := []struct {
		name string
		call func(c *streamkit.RoomClient, room string) error
		// ours is the answer expected from both servers, unless reference is set.
		ours, reference string
	}{
		{"create a room with no name", func(c *streamkit.RoomClient, _ string) error {
			_, err := c.CreateRoom(ctx, &pb.CreateRoomRequest{})
			return err
			// The reference creates a room with an empty name, which nobody can then join or address.
		}, "invalid_argument: no room name", "ok"},
		{"create a room with a long name", func(c *streamkit.RoomClient, _ string) error {
			_, err := c.CreateRoom(ctx, &pb.CreateRoomRequest{Name: long})
			return err
		}, "invalid_argument: room name length exceeds limits: max length 256", ""},
		{"delete a room that is not open", func(c *streamkit.RoomClient, room string) error {
			_, err := c.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: room + "-not-open"})
			return err
		}, notFoundRoom, ""},
		{"change metadata of a room that is not open", func(c *streamkit.RoomClient, room string) error {
			_, err := c.UpdateRoomMetadata(ctx, &pb.UpdateRoomMetadataRequest{Room: room + "-not-open", Metadata: "x"})
			return err
		}, notFoundRoom, ""},
		{"get a participant who is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "nobody"})
			return err
		}, notFoundParticipant, ""},
		{"remove a participant who is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.RemoveParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "nobody"})
			return err
		}, notFoundParticipant, ""},
		{"change a participant who is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.UpdateParticipant(ctx, &pb.UpdateParticipantRequest{Room: room, Identity: "nobody", Metadata: "x"})
			return err
		}, notFoundParticipant, ""},
		{"change a participant to a long name", func(c *streamkit.RoomClient, room string) error {
			_, err := c.UpdateParticipant(ctx, &pb.UpdateParticipantRequest{Room: room, Identity: "here", Name: long})
			return err
		}, "invalid_argument: name length exceeds limits 256", ""},
		{"mute a participant who is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.MutePublishedTrack(ctx, &pb.MuteRoomTrackRequest{Room: room, Identity: "nobody", TrackSid: "TR_x", Muted: true})
			return err
		}, notFoundParticipant, noResponse},
		{"mute a track that is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.MutePublishedTrack(ctx, &pb.MuteRoomTrackRequest{Room: room, Identity: "here", TrackSid: "TR_x", Muted: true})
			return err
			// The reference reports success, with no track in the response.
		}, "not_found: track is not found", "ok"},
		{"subscribe a participant who is not there", func(c *streamkit.RoomClient, room string) error {
			_, err := c.UpdateSubscriptions(ctx, &pb.UpdateSubscriptionsRequest{Room: room, Identity: "nobody", TrackSids: []string{"TR_x"}, Subscribe: true})
			return err
		}, notFoundParticipant, noResponse},
		{"send data with a short nonce", func(c *streamkit.RoomClient, room string) error {
			_, err := c.SendData(ctx, &pb.SendDataRequest{Room: room, Data: []byte("x"), Nonce: []byte("short")})
			return err
		}, "invalid_argument: nonce should be 16-bytes or not present, got: 5 bytes", ""},
		{"send data to a room that is not open", func(c *streamkit.RoomClient, room string) error {
			_, err := c.SendData(ctx, &pb.SendDataRequest{Room: room + "-not-open", Data: []byte("x")})
			return err
		}, notFoundRoom, noResponse},
		{"list participants of a room that is not open", func(c *streamkit.RoomClient, room string) error {
			_, err := c.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room + "-not-open"})
			return err
		}, "ok", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			servers := []struct {
				name string
				s    *fixture.Server
				want string
			}{{"ours", fx.Info.Ours, tc.ours}, {"reference", fx.Info.Reference, tc.reference}}
			if tc.reference == "" {
				servers[1].want = tc.ours
			}
			for _, server := range servers {
				tg := target{server.name, server.s, streamkit.NewClient(server.s.URL, server.s.APIKey, server.s.APISecret)}
				room := roomName(t)
				p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "here"})
				check(t, p.WaitConnected())
				if got := outcome(tc.call(tg.client.Room, room)); got != server.want {
					t.Errorf("%s:\n got: %s\nwant: %s", server.name, got, server.want)
				}
			}
		})
	}
}
