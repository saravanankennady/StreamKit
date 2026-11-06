package streamkit_test

// These tests run the SDK against real running servers: this server, and the reference server wherever a call exists
// there. A test that the reference fails tests the wrong thing. Participants are real: the streamkit CLI joining with a
// real WebRTC session and real media.
//
// Set STREAMKIT_TEST_DATABASE_URL to run them, and STREAMKIT_TEST_WORKERS=1 for the recording and ingest tests, which
// need Chrome and ffmpeg.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
	"elnova-streamkit.local/tests/sdk/fixture"
)

var fx *fixture.Fixture

func TestMain(m *testing.M) {
	if os.Getenv("STREAMKIT_TEST_DATABASE_URL") == "" {
		fmt.Println("STREAMKIT_TEST_DATABASE_URL is not set: only the tests that need no server will run")
		os.Exit(m.Run())
	}
	var err error
	fx, err = fixture.Start(context.Background(), fixture.Options{Workers: os.Getenv("STREAMKIT_TEST_WORKERS") != "", Log: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start the servers:", err)
		os.Exit(1)
	}
	code := m.Run()
	fx.Stop()
	os.Exit(code)
}

type target struct {
	name   string
	server *fixture.Server
	client *streamkit.Client
}

func (tg target) isReference() bool { return tg.name == "reference" }

// eachServer runs a test against this server and the reference.
func eachServer(t *testing.T, fn func(t *testing.T, tg target)) {
	t.Helper()
	if fx == nil {
		t.Skip("needs running servers")
	}
	run := func(name string, s *fixture.Server) {
		t.Run(name, func(t *testing.T) {
			if s == nil {
				t.Skip("the reference server is not available")
			}
			fn(t, target{name, s, streamkit.NewClient(s.URL, s.APIKey, s.APISecret)})
		})
	}
	run("ours", fx.Info.Ours)
	run("reference", fx.Info.Reference)
}

func ourServer(t *testing.T) target {
	t.Helper()
	if fx == nil {
		t.Skip("needs running servers")
	}
	s := fx.Info.Ours
	return target{"ours", s, streamkit.NewClient(s.URL, s.APIKey, s.APISecret)}
}

func withWorkers(t *testing.T) target {
	t.Helper()
	tg := ourServer(t)
	if !fx.Info.Workers {
		t.Skip("STREAMKIT_TEST_WORKERS is not set")
	}
	return tg
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

var roomCounter atomic.Int64

func roomName(t *testing.T) string {
	name := regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(t.Name(), "-")
	return fmt.Sprintf("sdk-%s-%d-%d", strings.ToLower(name), os.Getpid(), roomCounter.Add(1))
}

func must[T any](t *testing.T) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if streamkit.ErrorCode(err) != code {
		t.Errorf("want error code %s, got %v", code, err)
	}
}

func join(t *testing.T, ctx context.Context, tg target, room string, o fixture.PeerOptions) *fixture.Peer {
	t.Helper()
	p, err := fx.Join(ctx, tg.server, room, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func publishing(t *testing.T, ctx context.Context, tg target, room, identity string) (*fixture.Peer, []fixture.Event) {
	t.Helper()
	p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: identity, Publish: true})
	tracks, err := p.WaitPublished(2)
	if err != nil {
		t.Fatal(err)
	}
	return p, tracks
}

func trackOfKind(t *testing.T, tracks []fixture.Event, kind string) string {
	t.Helper()
	for _, e := range tracks {
		if e.Kind == kind {
			return e.SID
		}
	}
	t.Fatalf("no %s track in %v", kind, tracks)
	return ""
}

func TestRoomLifecycle(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		c := tg.client.Room
		room := roomName(t)
		created := must[*pb.Room](t)(c.CreateRoom(ctx, &pb.CreateRoomRequest{Name: room, EmptyTimeout: 120, DepartureTimeout: 30, MaxParticipants: 7, Metadata: "hello"}))
		if created.GetName() != room || created.GetEmptyTimeout() != 120 || created.GetDepartureTimeout() != 30 || created.GetMaxParticipants() != 7 || created.GetMetadata() != "hello" {
			t.Errorf("created: %v", created)
		}
		if !strings.HasPrefix(created.GetSid(), "RM_") || created.GetCreationTime() == 0 {
			t.Errorf("created: %v", created)
		}

		again := must[*pb.Room](t)(c.CreateRoom(ctx, &pb.CreateRoomRequest{Name: room}))
		if again.GetSid() != created.GetSid() {
			t.Errorf("creating an open room should return it: %s, %s", again.GetSid(), created.GetSid())
		}

		named := must[*pb.ListRoomsResponse](t)(c.ListRooms(ctx, &pb.ListRoomsRequest{Names: []string{room, room + "-not-open"}}))
		if len(named.GetRooms()) != 1 || named.GetRooms()[0].GetSid() != created.GetSid() {
			t.Errorf("by name: %v", named)
		}
		all := must[*pb.ListRoomsResponse](t)(c.ListRooms(ctx, &pb.ListRoomsRequest{}))
		if !slices.ContainsFunc(all.GetRooms(), func(r *pb.Room) bool { return r.GetName() == room }) {
			t.Errorf("all rooms: %v", all)
		}

		updated := must[*pb.Room](t)(c.UpdateRoomMetadata(ctx, &pb.UpdateRoomMetadataRequest{Room: room, Metadata: "changed"}))
		if updated.GetMetadata() != "changed" {
			t.Errorf("updated: %v", updated)
		}
		cleared := must[*pb.Room](t)(c.UpdateRoomMetadata(ctx, &pb.UpdateRoomMetadataRequest{Room: room, Metadata: ""}))
		if cleared.GetMetadata() != "" {
			t.Errorf("cleared: %v", cleared)
		}

		must[*pb.DeleteRoomResponse](t)(c.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: room}))
		after := must[*pb.ListRoomsResponse](t)(c.ListRooms(ctx, &pb.ListRoomsRequest{Names: []string{room}}))
		if len(after.GetRooms()) != 0 {
			t.Errorf("after delete: %v", after)
		}
	})
}

func TestRoomErrors(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		c := tg.client.Room
		missing := roomName(t)

		_, err := c.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: missing})
		wantCode(t, err, "not_found")

		_, err = c.UpdateRoomMetadata(ctx, &pb.UpdateRoomMetadataRequest{Room: missing, Metadata: "x"})
		if err == nil {
			t.Error("updating a room that does not exist should fail")
		}

		// A room nobody has opened has no participants, which is not an error.
		list := must[*pb.ListParticipantsResponse](t)(c.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: missing}))
		if len(list.GetParticipants()) != 0 {
			t.Errorf("participants: %v", list)
		}

		_, err = c.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: missing, Identity: "nobody"})
		wantCode(t, err, "not_found")

		_, err = c.RemoveParticipant(ctx, &pb.RoomParticipantIdentity{Room: missing, Identity: "nobody"})
		wantCode(t, err, "not_found")
	})
}

func TestPermissions(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		s := tg.server
		room := roomName(t)
		must[*pb.Room](t)(tg.client.Room.CreateRoom(ctx, &pb.CreateRoomRequest{Name: room}))
		t.Cleanup(func() { _, _ = tg.client.Room.DeleteRoom(context.Background(), &pb.DeleteRoomRequest{Room: room}) })

		withToken := func(grant *streamkit.VideoGrant) *streamkit.RoomClient {
			token, err := streamkit.NewAccessToken(s.APIKey, s.APISecret).SetIdentity("caller").SetVideoGrant(grant).ToJWT()
			check(t, err)
			return streamkit.NewRoomClient(s.URL, "", "", streamkit.WithToken(token))
		}

		// Every denial is the same code and text, so that a caller learns nothing about what exists.
		denied := func(err error) {
			t.Helper()
			var e *streamkit.Error
			if !errors.As(err, &e) || e.Code != "unauthenticated" || e.Status != http.StatusUnauthorized || e.Message != "permissions denied" {
				t.Errorf("want permissions denied, got %v", err)
			}
		}
		joinOnly := withToken(&streamkit.VideoGrant{RoomJoin: true, Room: room})
		_, err := joinOnly.CreateRoom(ctx, &pb.CreateRoomRequest{Name: room})
		denied(err)
		_, err = joinOnly.ListRooms(ctx, &pb.ListRoomsRequest{})
		denied(err)
		_, err = joinOnly.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: room})
		denied(err)
		_, err = joinOnly.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room})
		denied(err)

		otherRoom := withToken(&streamkit.VideoGrant{RoomAdmin: true, Room: room + "-other"})
		_, err = otherRoom.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room})
		denied(err)
		_, err = otherRoom.SendData(ctx, &pb.SendDataRequest{Room: room, Data: []byte("x")})
		denied(err)

		// A right grant works, so the denials above are about the grant.
		must[*pb.ListRoomsResponse](t)(withToken(&streamkit.VideoGrant{RoomList: true}).ListRooms(ctx, &pb.ListRoomsRequest{}))
		must[*pb.ListParticipantsResponse](t)(withToken(&streamkit.VideoGrant{RoomAdmin: true, Room: room}).ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room}))

		// Bad credentials are answered in plain text, outside the Twirp error shape.
		_, err = streamkit.NewRoomClient(s.URL, s.APIKey, "not-the-secret").ListRooms(ctx, &pb.ListRoomsRequest{})
		wantCode(t, err, "unauthenticated")
		_, err = streamkit.NewRoomClient(s.URL, "not-a-key", s.APISecret).ListRooms(ctx, &pb.ListRoomsRequest{})
		var e *streamkit.Error
		if !errors.As(err, &e) || e.Code != "unauthenticated" || e.Message != "invalid API key" {
			t.Errorf("unknown key: %v", err)
		}
		_, err = streamkit.NewRoomClient(s.URL, "", "").ListRooms(ctx, &pb.ListRoomsRequest{})
		if !errors.Is(err, streamkit.ErrMissingCredentials) {
			t.Errorf("no credentials: %v", err)
		}
	})
}

// Tokens from this SDK are accepted by both servers, and carry what they say.
func TestSDKTokenJoins(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		token := must[string](t)(streamkit.NewAccessToken(tg.server.APIKey, tg.server.APISecret).
			SetIdentity("token-holder").SetName("Token Holder").SetMetadata("token metadata").
			SetAttributes(map[string]string{"color": "green"}).
			SetVideoGrant(&streamkit.VideoGrant{RoomJoin: true, Room: room}).ToJWT())
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "token-holder", Token: token})
		joined := must[fixture.Event](t)(p.Wait(30*time.Second, "joining", func(e fixture.Event) bool { return e.Event == "joined" }))
		if joined.Identity != "token-holder" || joined.Name != "Token Holder" || joined.Room != room || joined.Attributes["color"] != "green" {
			t.Errorf("joined: %+v", joined)
		}
		info := must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "token-holder"}))
		if info.GetMetadata() != "token metadata" || info.GetName() != "Token Holder" || info.GetAttributes()["color"] != "green" {
			t.Errorf("participant: %v", info)
		}
	})
}

// A token with every grant still joins: servers ignore what they do not act on, and accept the rest.
func TestTokenWithEveryGrantJoins(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		video := &streamkit.VideoGrant{
			RoomJoin: true, Room: room, RoomCreate: true, RoomList: true, RoomRecord: true, RoomAdmin: true, IngressAdmin: true,
			CanPublish: streamkit.Bool(true), CanSubscribe: streamkit.Bool(true), CanPublishData: streamkit.Bool(true),
			CanUpdateOwnMetadata: streamkit.Bool(true), CanSubscribeMetrics: streamkit.Bool(true), CanManageAgentSession: streamkit.Bool(true),
			DestinationRoom: room + "-dest",
		}
		video.SetCanPublishSources(pb.TrackSource_CAMERA, pb.TrackSource_MICROPHONE)
		token := must[string](t)(streamkit.NewAccessToken(tg.server.APIKey, tg.server.APISecret).
			SetIdentity("everything").SetVideoGrant(video).
			SetSIPGrant(&streamkit.SIPGrant{Admin: true, Call: true}).
			SetAgentGrant(&streamkit.AgentGrant{Admin: true, DispatchAdmin: true}).
			SetInferenceGrant(&streamkit.InferenceGrant{Perform: true}).
			SetObservabilityGrant(&streamkit.ObservabilityGrant{Write: true}).
			SetAgentEndpointGrant(&streamkit.AgentEndpointGrant{Call: true}).
			SetAgentThreadGrant(&streamkit.AgentThreadGrant{All: true}).
			ToJWT())
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "everything", Token: token})
		check(t, p.WaitConnected())
		// The same token is good for the API too.
		caller := streamkit.NewRoomClient(tg.server.URL, "", "", streamkit.WithToken(token))
		list := must[*pb.ListParticipantsResponse](t)(caller.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room}))
		if len(list.GetParticipants()) != 1 {
			t.Errorf("participants: %v", list)
		}
		perm := list.GetParticipants()[0].GetPermission()
		if !perm.GetCanPublish() || !perm.GetCanUpdateMetadata() || len(perm.GetCanPublishSources()) != 2 {
			t.Errorf("permission: %v", perm)
		}
	})
}

// The grants a participant's token carries are what the server applies to them.
func TestTokenGrantsApply(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		token := must[string](t)(streamkit.NewAccessToken(tg.server.APIKey, tg.server.APISecret).
			SetIdentity("limited").SetKind(pb.ParticipantInfo_AGENT).
			SetVideoGrant(&streamkit.VideoGrant{RoomJoin: true, Room: room, CanPublish: streamkit.Bool(false), CanPublishData: streamkit.Bool(false)}).ToJWT())
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "limited", Token: token})
		check(t, p.WaitConnected())
		info := must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "limited"}))
		perm := info.GetPermission()
		if perm.GetCanPublish() || perm.GetCanPublishData() || !perm.GetCanSubscribe() {
			t.Errorf("permission: %v", perm)
		}
		if info.GetKind() != pb.ParticipantInfo_AGENT {
			t.Errorf("kind: %v", info.GetKind())
		}
	})
}

// Each permission a token leaves unset takes the protocol's default.
func TestTokenGrantDefaults(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		permission := func(identity string, grant *streamkit.VideoGrant) *pb.ParticipantPermission {
			t.Helper()
			grant.RoomJoin, grant.Room = true, room
			token := must[string](t)(streamkit.NewAccessToken(tg.server.APIKey, tg.server.APISecret).SetIdentity(identity).SetVideoGrant(grant).ToJWT())
			p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: identity, Token: token})
			check(t, p.WaitConnected())
			return must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: identity})).GetPermission()
		}
		open := permission("open", &streamkit.VideoGrant{})
		if !open.GetCanPublish() || !open.GetCanSubscribe() || !open.GetCanPublishData() || open.GetCanUpdateMetadata() {
			t.Errorf("unset: %v", open)
		}
		// Data follows publishing, unless a token says otherwise.
		noPublish := permission("no-publish", &streamkit.VideoGrant{CanPublish: streamkit.Bool(false)})
		if noPublish.GetCanPublish() || noPublish.GetCanPublishData() || !noPublish.GetCanSubscribe() {
			t.Errorf("publish denied: %v", noPublish)
		}
		dataOnly := permission("data-only", &streamkit.VideoGrant{CanPublish: streamkit.Bool(false), CanPublishData: streamkit.Bool(true)})
		if dataOnly.GetCanPublish() || !dataOnly.GetCanPublishData() {
			t.Errorf("data allowed: %v", dataOnly)
		}
		noData := permission("no-data", &streamkit.VideoGrant{CanPublishData: streamkit.Bool(false)})
		if !noData.GetCanPublish() || noData.GetCanPublishData() {
			t.Errorf("data denied: %v", noData)
		}
		sources := permission("camera-only", &streamkit.VideoGrant{CanPublishSources: []string{"camera"}})
		if !slices.Equal(sources.GetCanPublishSources(), []pb.TrackSource{pb.TrackSource_CAMERA}) {
			t.Errorf("sources: %v", sources)
		}
		hidden := permission("hidden", &streamkit.VideoGrant{Hidden: true})
		if !hidden.GetHidden() {
			t.Errorf("hidden: %v", hidden)
		}
	})
}

// A join token's room configuration applies when that join creates the room.
func TestTokenRoomConfig(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		token := must[string](t)(streamkit.NewAccessToken(tg.server.APIKey, tg.server.APISecret).
			SetIdentity("configured").
			SetVideoGrant(&streamkit.VideoGrant{RoomJoin: true, Room: room}).
			SetRoomConfig(&pb.RoomConfiguration{EmptyTimeout: 77, DepartureTimeout: 66, MaxParticipants: 55}).
			ToJWT())
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "configured", Token: token})
		check(t, p.WaitConnected())
		list := must[*pb.ListRoomsResponse](t)(tg.client.Room.ListRooms(ctx, &pb.ListRoomsRequest{Names: []string{room}}))
		if len(list.GetRooms()) != 1 {
			t.Fatalf("rooms: %v", list)
		}
		r := list.GetRooms()[0]
		if r.GetEmptyTimeout() != 77 || r.GetDepartureTimeout() != 66 || r.GetMaxParticipants() != 55 {
			t.Errorf("room did not take the token's configuration: %v", r)
		}
	})
}

// Tokens from the reference server's own tool are accepted by both servers, and verified by this SDK.
func TestReferenceToolToken(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		if fx.Info.ReferenceBinary == "" {
			t.Skip("the reference server is not available")
		}
		ctx := testContext(t)
		room := roomName(t)
		token := must[string](t)(fx.ReferenceJoinToken(ctx, tg.server, room, "from-the-tool"))
		claims := must[*streamkit.Claims](t)(streamkit.VerifyToken(token, streamkit.Keys{tg.server.APIKey: tg.server.APISecret}))
		if claims.Identity != "from-the-tool" || claims.Video.Room != room || !claims.Video.RoomJoin {
			t.Errorf("claims: %+v", claims)
		}
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "from-the-tool", Token: token})
		check(t, p.WaitConnected())
	})
}

func TestParticipantsWithRealMedia(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		_, tracks := publishing(t, ctx, tg, room, "publisher")
		subscriber := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "subscriber"})
		for _, kind := range []string{"video", "audio"} {
			_, err := subscriber.Wait(30*time.Second, "real "+kind, func(e fixture.Event) bool {
				return e.Event == "media" && e.Kind == kind && e.Packets > 10 && e.Bytes > 0
			})
			check(t, err)
		}

		list := must[*pb.ListParticipantsResponse](t)(tg.client.Room.ListParticipants(ctx, &pb.ListParticipantsRequest{Room: room}))
		var identities []string
		for _, p := range list.GetParticipants() {
			identities = append(identities, p.GetIdentity())
		}
		slices.Sort(identities)
		if !slices.Equal(identities, []string{"publisher", "subscriber"}) {
			t.Errorf("participants: %v", identities)
		}

		info := must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "publisher"}))
		if info.GetState() != pb.ParticipantInfo_ACTIVE || !strings.HasPrefix(info.GetSid(), "PA_") {
			t.Errorf("info: %v", info)
		}
		var sids []string
		for _, track := range info.GetTracks() {
			sids = append(sids, track.GetSid())
		}
		for _, e := range tracks {
			if !slices.Contains(sids, e.SID) {
				t.Errorf("track %s the server accepted is not in %v", e.SID, sids)
			}
		}
		if !info.GetIsPublisher() || info.GetJoinedAt() == 0 {
			t.Errorf("info: %v", info)
		}
		for _, track := range info.GetTracks() {
			switch track.GetType() {
			case pb.TrackType_VIDEO:
				if track.GetSource() != pb.TrackSource_CAMERA || track.GetWidth() != 640 || track.GetHeight() != 360 || !strings.EqualFold(track.GetMimeType(), "video/vp8") {
					t.Errorf("video track: %v", track)
				}
			case pb.TrackType_AUDIO:
				if track.GetSource() != pb.TrackSource_MICROPHONE || !strings.EqualFold(track.GetMimeType(), "audio/opus") {
					t.Errorf("audio track: %v", track)
				}
			default:
				t.Errorf("track: %v", track)
			}
		}
		// The reference brings these counts up to date every few seconds, so this waits for them.
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
			rooms := must[*pb.ListRoomsResponse](t)(tg.client.Room.ListRooms(ctx, &pb.ListRoomsRequest{Names: []string{room}}))
			if len(rooms.GetRooms()) == 1 && rooms.GetRooms()[0].GetNumParticipants() == 2 && rooms.GetRooms()[0].GetNumPublishers() == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("rooms: %v", rooms)
			}
		}

		_, err := tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "nobody"})
		wantCode(t, err, "not_found")
		var e *streamkit.Error
		if errors.As(err, &e) && e.Message != "participant does not exist" {
			t.Errorf("message: %q", e.Message)
		}
	})
}

func TestMutePublishedTrack(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		p, tracks := publishing(t, ctx, tg, room, "publisher")
		audio := trackOfKind(t, tracks, "audio")

		res := must[*pb.MuteRoomTrackResponse](t)(tg.client.Room.MutePublishedTrack(ctx, &pb.MuteRoomTrackRequest{Room: room, Identity: "publisher", TrackSid: audio, Muted: true}))
		if !res.GetTrack().GetMuted() || res.GetTrack().GetSid() != audio {
			t.Errorf("mute: %v", res)
		}
		_, err := p.Wait(15*time.Second, "being told it was muted", func(e fixture.Event) bool {
			return e.Event == "muted" && e.SID == audio && e.Muted != nil && *e.Muted
		})
		check(t, err)
		// A server lists a track once media has arrived for it, which can be a moment after it accepted the track.
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			info := must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "publisher"}))
			i := slices.IndexFunc(info.GetTracks(), func(track *pb.TrackInfo) bool { return track.GetSid() == audio })
			if i >= 0 {
				if !info.GetTracks()[i].GetMuted() {
					t.Errorf("track: %v", info.GetTracks()[i])
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the muted track was never listed: %v", info)
			}
		}

		// Unmuting from afar is off unless the server turned it on.
		_, err = tg.client.Room.MutePublishedTrack(ctx, &pb.MuteRoomTrackRequest{Room: room, Identity: "publisher", TrackSid: audio, Muted: false})
		wantCode(t, err, "failed_precondition")
	})
}

func TestUpdateParticipant(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		target := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "target"})
		check(t, target.WaitConnected())
		observer := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "observer"})
		check(t, observer.WaitConnected())

		res := must[*pb.ParticipantInfo](t)(tg.client.Room.UpdateParticipant(ctx, &pb.UpdateParticipantRequest{
			Room: room, Identity: "target", Metadata: "new metadata", Name: "New Name", Attributes: map[string]string{"a": "1"},
		}))
		if res.GetMetadata() != "new metadata" || res.GetName() != "New Name" || res.GetAttributes()["a"] != "1" {
			t.Errorf("response: %v", res)
		}
		for _, p := range []*fixture.Peer{target, observer} {
			_, err := p.Wait(15*time.Second, "the update", func(e fixture.Event) bool {
				return e.Event == "participant_update" && e.Identity == "target" && e.Metadata != nil && *e.Metadata == "new metadata" && e.Name == "New Name" && e.Attributes["a"] == "1"
			})
			check(t, err)
		}

		// A permission replaces the whole set.
		res = must[*pb.ParticipantInfo](t)(tg.client.Room.UpdateParticipant(ctx, &pb.UpdateParticipantRequest{
			Room: room, Identity: "target", Permission: &pb.ParticipantPermission{CanSubscribe: true},
		}))
		if res.GetPermission().GetCanPublish() || !res.GetPermission().GetCanSubscribe() {
			t.Errorf("permission: %v", res.GetPermission())
		}
		if res.GetMetadata() != "new metadata" {
			t.Errorf("empty metadata should leave it unchanged: %v", res)
		}
		_, err := target.Wait(15*time.Second, "the permission update", func(e fixture.Event) bool {
			return e.Event == "participant_update" && e.Identity == "target" && e.CanPublish != nil && !*e.CanPublish
		})
		check(t, err)

		_, err = tg.client.Room.UpdateParticipant(ctx, &pb.UpdateParticipantRequest{Room: room, Identity: "nobody", Metadata: "x"})
		wantCode(t, err, "not_found")
	})
}

func TestUpdateSubscriptions(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		_, tracks := publishing(t, ctx, tg, room, "publisher")
		video, audio := trackOfKind(t, tracks, "video"), trackOfKind(t, tracks, "audio")
		viewer := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "viewer", NoAutoSubscribe: true})
		_, err := viewer.Wait(30*time.Second, "joining", func(e fixture.Event) bool { return e.Event == "joined" })
		check(t, err)

		must[*pb.UpdateSubscriptionsResponse](t)(tg.client.Room.UpdateSubscriptions(ctx, &pb.UpdateSubscriptionsRequest{
			Room: room, Identity: "viewer", TrackSids: []string{video}, Subscribe: true,
		}))
		_, err = viewer.Wait(30*time.Second, "video after subscribing", func(e fixture.Event) bool {
			return e.Event == "media" && e.Kind == "video" && e.Packets > 10
		})
		check(t, err)
		for _, e := range viewer.Events() {
			if e.Event == "media" && e.Kind == "audio" {
				t.Errorf("audio arrived with no subscription: %+v", e)
			}
		}

		must[*pb.UpdateSubscriptionsResponse](t)(tg.client.Room.UpdateSubscriptions(ctx, &pb.UpdateSubscriptionsRequest{
			Room: room, Identity: "viewer", ParticipantTracks: []*pb.ParticipantTracks{{ParticipantSid: "publisher", TrackSids: []string{audio}}}, Subscribe: true,
		}))
		_, err = viewer.Wait(30*time.Second, "audio after subscribing", func(e fixture.Event) bool {
			return e.Event == "media" && e.Kind == "audio" && e.Packets > 10
		})
		check(t, err)

		must[*pb.UpdateSubscriptionsResponse](t)(tg.client.Room.UpdateSubscriptions(ctx, &pb.UpdateSubscriptionsRequest{
			Room: room, Identity: "viewer", TrackSids: []string{video}, Subscribe: false,
		}))
		if !mediaStops(viewer, video, 15*time.Second) {
			t.Error("video kept arriving after unsubscribing")
		}
	})
}

// mediaStops reports whether the media reports for one kind stop coming. A report comes once a second while packets
// arrive, so three seconds without one means they have stopped.
func mediaStops(p *fixture.Peer, sid string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	last := int64(-1)
	since := time.Now()
	for time.Now().Before(deadline) {
		var packets int64
		for _, e := range p.Events() {
			if e.Event == "media" && e.Kind == "video" {
				packets = e.Packets
			}
		}
		if packets != last {
			last, since = packets, time.Now()
		} else if time.Since(since) > 3*time.Second {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func dataEvent(payload string) func(fixture.Event) bool {
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	return func(e fixture.Event) bool { return e.Event == "data" && e.Data == encoded }
}

func TestSendData(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		alice := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "alice"})
		check(t, alice.WaitDataChannels())
		bob := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "bob"})
		check(t, bob.WaitDataChannels())
		send := func(in *pb.SendDataRequest) {
			t.Helper()
			in.Room = room
			must[*pb.SendDataResponse](t)(tg.client.Room.SendData(ctx, in))
		}
		topic := "news"
		send(&pb.SendDataRequest{Data: []byte("to everyone"), Kind: pb.DataPacket_RELIABLE, Topic: &topic})
		for _, p := range []*fixture.Peer{alice, bob} {
			e := must[fixture.Event](t)(p.Wait(15*time.Second, "the broadcast", dataEvent("to everyone")))
			if e.Topic != "news" || e.Channel != "_reliable" {
				t.Errorf("%s got %+v", p.Identity, e)
			}
		}

		send(&pb.SendDataRequest{Data: []byte("for bob"), Kind: pb.DataPacket_RELIABLE, DestinationIdentities: []string{"bob"}})
		must[fixture.Event](t)(bob.Wait(15*time.Second, "its own message", dataEvent("for bob")))

		// Reliable data arrives in order, so once alice has this, she would have had the other if it was coming.
		send(&pb.SendDataRequest{Data: []byte("after"), Kind: pb.DataPacket_RELIABLE})
		must[fixture.Event](t)(alice.Wait(15*time.Second, "the later message", dataEvent("after")))
		for _, e := range alice.Events() {
			if dataEvent("for bob")(e) {
				t.Error("alice received a message for bob")
			}
		}

		send(&pb.SendDataRequest{Data: []byte("lossy"), Kind: pb.DataPacket_LOSSY})
		e := must[fixture.Event](t)(bob.Wait(15*time.Second, "the lossy message", dataEvent("lossy")))
		if e.Channel != "_lossy" {
			t.Errorf("lossy data came on %s", e.Channel)
		}

		send(&pb.SendDataRequest{Data: []byte("with a nonce"), Kind: pb.DataPacket_RELIABLE, Nonce: bytes.Repeat([]byte{7}, 16)})
		must[fixture.Event](t)(bob.Wait(15*time.Second, "the message with a nonce", dataEvent("with a nonce")))
		_, err := tg.client.Room.SendData(ctx, &pb.SendDataRequest{Room: room, Data: []byte("x"), Nonce: []byte("short")})
		wantCode(t, err, "invalid_argument")
	})
}

func TestRemoveParticipant(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "leaving"})
		check(t, p.WaitConnected())
		must[*pb.RemoveParticipantResponse](t)(tg.client.Room.RemoveParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "leaving"}))
		left := must[fixture.Event](t)(p.Wait(15*time.Second, "being removed", func(e fixture.Event) bool { return e.Event == "left" }))
		if left.Reason != "PARTICIPANT_REMOVED" {
			t.Errorf("reason: %q", left.Reason)
		}
		_, err := tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "leaving"})
		wantCode(t, err, "not_found")
		_, err = tg.client.Room.RemoveParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "leaving"})
		wantCode(t, err, "not_found")
	})
}

func TestDeleteRoomDisconnects(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "inside"})
		check(t, p.WaitConnected())
		must[*pb.DeleteRoomResponse](t)(tg.client.Room.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: room}))
		left := must[fixture.Event](t)(p.Wait(15*time.Second, "the room closing", func(e fixture.Event) bool { return e.Event == "left" }))
		if left.Reason != "ROOM_DELETED" {
			t.Errorf("reason: %q", left.Reason)
		}
	})
}

func TestRoomMetadataReachesParticipants(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		p := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "inside"})
		check(t, p.WaitConnected())
		must[*pb.Room](t)(tg.client.Room.UpdateRoomMetadata(ctx, &pb.UpdateRoomMetadataRequest{Room: room, Metadata: "for those inside"}))
		_, err := p.Wait(15*time.Second, "the room update", func(e fixture.Event) bool {
			return e.Event == "room_update" && e.Metadata != nil && *e.Metadata == "for those inside"
		})
		check(t, err)
	})
}

// What this server has not implemented fails clearly. The reference has PerformRpc, and refuses forward and move.
func TestMethodsThatAreNotImplemented(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		_, forward := tg.client.Room.ForwardParticipant(ctx, &pb.ForwardParticipantRequest{Room: room, Identity: "x", DestinationRoom: room + "-2"})
		_, move := tg.client.Room.MoveParticipant(ctx, &pb.MoveParticipantRequest{Room: room, Identity: "x", DestinationRoom: room + "-2"})
		if forward == nil || move == nil {
			t.Errorf("forward: %v, move: %v", forward, move)
		}
		_, rpc := tg.client.Room.PerformRpc(ctx, &pb.PerformRpcRequest{Room: room, Method: "m"})
		if tg.isReference() {
			wantCode(t, rpc, "invalid_argument")
			return
		}
		for name, err := range map[string]error{"forward": forward, "move": move, "rpc": rpc} {
			if !errors.Is(err, streamkit.ErrNotSupported) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

func TestAgentDispatch(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		c := tg.client.AgentDispatch
		created, err := c.CreateDispatch(ctx, &pb.CreateAgentDispatchRequest{AgentName: "helper", Room: room, Metadata: "m"})
		if !tg.isReference() {
			if !errors.Is(err, streamkit.ErrNotSupported) {
				t.Fatalf("want a clear refusal, got %v", err)
			}
			if !strings.Contains(err.Error(), "no AgentDispatchService service") {
				t.Errorf("message: %v", err)
			}
			return
		}
		check(t, err)
		if created.GetAgentName() != "helper" || created.GetRoom() != room || !strings.HasPrefix(created.GetId(), "AD_") {
			t.Errorf("created: %v", created)
		}
		list := must[*pb.ListAgentDispatchResponse](t)(c.ListDispatch(ctx, &pb.ListAgentDispatchRequest{Room: room}))
		// The reference also keeps a dispatch of its own in every room, for agents that take any job.
		if !slices.ContainsFunc(list.GetAgentDispatches(), func(d *pb.AgentDispatch) bool { return d.GetId() == created.GetId() }) {
			t.Errorf("list: %v", list)
		}
		must[*pb.AgentDispatch](t)(c.DeleteDispatch(ctx, &pb.DeleteAgentDispatchRequest{DispatchId: created.GetId(), Room: room}))
	})
}

// Real deliveries from each server pass this SDK's receiver, byte for byte, and a changed byte does not.
func TestWebhooksFromRealServers(t *testing.T) {
	eachServer(t, func(t *testing.T, tg target) {
		ctx := testContext(t)
		room := roomName(t)
		receiver := streamkit.NewWebhookReceiver(streamkit.Keys{tg.server.APIKey: tg.server.APISecret})
		// seen checks every delivery for this room so far, and says which events have arrived.
		seen := func() (map[string]bool, fixture.Captured) {
			t.Helper()
			events := map[string]bool{}
			var last fixture.Captured
			for _, c := range must[[]fixture.Captured](t)(fx.CapturedWebhooks(tg.name)) {
				event, err := receiver.Receive(c.Body, c.Authorization)
				if err != nil {
					t.Fatalf("a real delivery failed the check: %v\n%s", err, c.Body)
				}
				if event.GetRoom().GetName() != room {
					continue
				}
				if event.GetId() == "" || event.GetCreatedAt() == 0 {
					t.Errorf("event: %v", event)
				}
				if c.ContentType != "application/webhook+json" {
					t.Errorf("content type: %q", c.ContentType)
				}
				events[event.GetEvent()] = true
				last = c
			}
			return events, last
		}
		waitFor := func(names ...string) fixture.Captured {
			t.Helper()
			deadline := time.Now().Add(45 * time.Second)
			for {
				events, last := seen()
				missing := slices.DeleteFunc(slices.Clone(names), func(name string) bool { return events[name] })
				if len(missing) == 0 {
					return last
				}
				if time.Now().After(deadline) {
					t.Fatalf("never received %v. Saw %v", missing, events)
				}
				time.Sleep(250 * time.Millisecond)
			}
		}

		publishing(t, ctx, tg, room, "webhook-publisher")
		// A participant counts as joined once its media is connected, and a track as published once media arrives.
		waitFor("room_started", "participant_joined", "track_published")
		must[*pb.DeleteRoomResponse](t)(tg.client.Room.DeleteRoom(ctx, &pb.DeleteRoomRequest{Room: room}))
		last := waitFor("room_finished", "participant_left")

		tampered := bytes.Replace(last.Body, []byte(room), []byte(room+"x"), 1)
		if _, err := receiver.Receive(tampered, last.Authorization); !errors.Is(err, streamkit.ErrWebhookBadChecksum) {
			t.Errorf("tampered: %v", err)
		}
		if _, err := streamkit.NewWebhookReceiver(streamkit.Keys{tg.server.APIKey: "not the secret"}).Receive(last.Body, last.Authorization); !errors.Is(err, streamkit.ErrInvalidToken) {
			t.Errorf("wrong secret: %v", err)
		}
	})
}

func TestEgressAndIngressNeedThisServersAPI(t *testing.T) {
	if fx == nil {
		t.Skip("needs running servers")
	}
	ctx := testContext(t)
	for name, s := range map[string]*fixture.Server{"reference": fx.Info.Reference, "plain": fx.Info.Plain} {
		t.Run(name, func(t *testing.T) {
			if s == nil {
				t.Skip("the reference server is not available")
			}
			c := streamkit.NewClient(s.URL, s.APIKey, s.APISecret)
			_, err := c.Egress.ListEgress(ctx, streamkit.ListOptions{})
			if !errors.Is(err, streamkit.ErrNotSupported) {
				t.Errorf("egress: %v", err)
			}
			_, err = c.Ingress.ListIngress(ctx)
			if !errors.Is(err, streamkit.ErrNotSupported) {
				t.Errorf("ingress: %v", err)
			}
		})
	}
}

func TestEgressUnsupportedKinds(t *testing.T) {
	// These answer without calling anything, so they need no server.
	c := streamkit.NewEgressClient("http://127.0.0.1:1", "k", "s")
	ctx := context.Background()
	for name, err := range map[string]error{
		"web":             second(c.StartWebEgress(ctx)),
		"track composite": second(c.StartTrackCompositeEgress(ctx)),
		"track":           second(c.StartTrackEgress(ctx)),
		"layout":          second(c.UpdateLayout(ctx, "id", "grid")),
		"stream":          second(c.UpdateStream(ctx, "id", nil, nil)),
	} {
		if !errors.Is(err, streamkit.ErrNotSupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestEgressValidation(t *testing.T) {
	tg := ourServer(t)
	ctx := testContext(t)
	if !fx.Info.Workers {
		// With no worker, the server says so before anything else.
		_, err := tg.client.Egress.StartRoomCompositeEgress(ctx, roomName(t), streamkit.EgressOutput{Format: "mp4"})
		wantCode(t, err, "egress_unavailable")
		return
	}
	_, err := tg.client.Egress.StartRoomCompositeEgress(ctx, roomName(t), streamkit.EgressOutput{Format: "mp4"})
	wantCode(t, err, "room_not_found")
	_, err = tg.client.Egress.StartRoomCompositeEgress(ctx, "r", streamkit.EgressOutput{})
	wantCode(t, err, "validation_failed")
	var e *streamkit.Error
	if errors.As(err, &e) && e.Fields["output"] == "" {
		t.Errorf("fields: %v", e.Fields)
	}
	_, err = tg.client.Egress.GetEgress(ctx, "EG_not_a_job")
	wantCode(t, err, "not_found")
}

// A whole recording: a real publisher, a real worker with a real browser, and a real file at the end.
func TestRecordingEndToEnd(t *testing.T) {
	tg := withWorkers(t)
	ctx := testContext(t)
	room := roomName(t)
	publishing(t, ctx, tg, room, "on-camera")
	c := tg.client.Egress

	job := must[*streamkit.Egress](t)(c.StartRoomCompositeEgress(ctx, room, streamkit.EgressOutput{Format: "mp4", Layout: "grid"}))
	if !strings.HasPrefix(job.ID, "EG_") || job.RoomName != room || job.Type != streamkit.EgressRoomComposite || job.Format != "mp4" {
		t.Errorf("job: %+v", job)
	}
	active := must[*streamkit.Egress](t)(c.WaitForEgress(ctx, job.ID, func(e *streamkit.Egress) bool { return e.Status == streamkit.EgressActive || e.Ended() }))
	if active.Status != streamkit.EgressActive {
		t.Fatalf("job ended early: %+v", active)
	}
	page := must[*streamkit.EgressPage](t)(c.ListEgress(ctx, streamkit.ListOptions{Limit: 10}))
	if !slices.ContainsFunc(page.Items, func(e *streamkit.Egress) bool { return e.ID == job.ID }) {
		t.Errorf("list: %+v", page)
	}
	_, err := c.RetryRecording(ctx, job.ID)
	wantCode(t, err, "egress_running")
	err = c.DeleteRecording(ctx, job.ID)
	wantCode(t, err, "egress_running")

	// Let it record something worth checking.
	time.Sleep(5 * time.Second)
	must[*streamkit.Egress](t)(c.StopEgress(ctx, job.ID))
	done := must[*streamkit.Egress](t)(c.WaitForEgress(ctx, job.ID, (*streamkit.Egress).Ended))
	if done.Status != streamkit.EgressComplete || done.FileSize == 0 || done.DurationMs == 0 {
		t.Fatalf("job: %+v", done)
	}
	_, err = c.StopEgress(ctx, job.ID)
	wantCode(t, err, "egress_not_running")

	recordings := must[*streamkit.EgressPage](t)(c.ListRecordings(ctx, streamkit.ListOptions{}))
	if !slices.ContainsFunc(recordings.Items, func(e *streamkit.Egress) bool { return e.ID == job.ID }) {
		t.Errorf("recordings: %+v", recordings)
	}
	must[*streamkit.Egress](t)(c.GetRecording(ctx, job.ID))

	path := t.TempDir() + "/recording.mp4"
	f := must[*os.File](t)(os.Create(path))
	n := must[int64](t)(c.DownloadRecording(ctx, job.ID, f))
	check(t, f.Close())
	if n != done.FileSize {
		t.Errorf("downloaded %d bytes of %d", n, done.FileSize)
	}
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "video") {
		t.Errorf("the file is not a video: %v: %s", err, out)
	}

	check(t, c.DeleteRecording(ctx, job.ID))
	_, err = c.GetEgress(ctx, job.ID)
	wantCode(t, err, "not_found")
}

// A stream output, from one room, through a real RTMP connection and a real ingress, into another room, where a real
// participant receives it. It also proves a job does not give away the stream key in its URL.
func TestStreamOutputEndToEnd(t *testing.T) {
	tg := withWorkers(t)
	ctx := testContext(t)
	from, to := roomName(t), roomName(t)
	publishing(t, ctx, tg, from, "on-camera")
	in := must[*streamkit.Ingress](t)(tg.client.Ingress.CreateIngress(ctx, &streamkit.CreateIngressRequest{InputType: streamkit.IngressRTMP, Room: to, ParticipantIdentity: "from-the-other-room"}))
	t.Cleanup(func() { _ = tg.client.Ingress.DeleteIngress(context.Background(), in.ID) })

	job := must[*streamkit.Egress](t)(tg.client.Egress.StartRoomCompositeEgress(ctx, from, streamkit.EgressOutput{StreamURLs: []string{"rtmp://" + fx.Info.RTMP + "/live/" + in.StreamKey}}))
	if job.Format != "" || job.OutputURL != "" {
		t.Errorf("a stream is not a recording: %+v", job)
	}
	if len(job.StreamURLs) != 1 || strings.Contains(job.StreamURLs[0], in.StreamKey) {
		t.Errorf("stream URLs: %v", job.StreamURLs)
	}
	viewer := join(t, ctx, tg, to, fixture.PeerOptions{Identity: "viewer"})
	_, err := viewer.Wait(90*time.Second, "the other room's stream", func(e fixture.Event) bool {
		return e.Event == "media" && e.Kind == "video" && e.Packets > 10
	})
	check(t, err)
	must[*streamkit.Egress](t)(tg.client.Egress.StopEgress(ctx, job.ID))
	done := must[*streamkit.Egress](t)(tg.client.Egress.WaitForEgress(ctx, job.ID, (*streamkit.Egress).Ended))
	if done.Status != streamkit.EgressComplete {
		t.Errorf("job: %+v", done)
	}
}

// A participant egress, stopped before it records anything worth keeping, to prove the other start calls reach the
// worker as asked.
func TestParticipantEgress(t *testing.T) {
	tg := withWorkers(t)
	ctx := testContext(t)
	room := roomName(t)
	publishing(t, ctx, tg, room, "on-camera")
	c := tg.client.Egress
	job := must[*streamkit.Egress](t)(c.StartParticipantEgress(ctx, room, "on-camera", streamkit.EgressOutput{Format: "mp4"}))
	if job.Type != streamkit.EgressParticipant {
		t.Errorf("job: %+v", job)
	}
	audio := must[*streamkit.Egress](t)(c.StartAudioOnlyEgress(ctx, room, "", streamkit.EgressOutput{Format: "mp4"}))
	if audio.Type != streamkit.EgressAudioOnly {
		t.Errorf("job: %+v", audio)
	}
	for _, id := range []string{job.ID, audio.ID} {
		must[*streamkit.Egress](t)(c.WaitForEgress(ctx, id, func(e *streamkit.Egress) bool { return e.Status == streamkit.EgressActive || e.Ended() }))
		time.Sleep(2 * time.Second)
		must[*streamkit.Egress](t)(c.StopEgress(ctx, id))
	}
	for _, id := range []string{job.ID, audio.ID} {
		done := must[*streamkit.Egress](t)(c.WaitForEgress(ctx, id, (*streamkit.Egress).Ended))
		if done.Status != streamkit.EgressComplete || done.FileSize == 0 {
			t.Errorf("job: %+v", done)
		}
	}
}

func TestIngressConfiguration(t *testing.T) {
	tg := ourServer(t)
	ctx := testContext(t)
	room := roomName(t)
	c := tg.client.Ingress

	created := must[*streamkit.Ingress](t)(c.CreateIngress(ctx, &streamkit.CreateIngressRequest{
		InputType: streamkit.IngressRTMP, Name: "stage", Room: room, ParticipantIdentity: "stage", ParticipantName: "Stage",
	}))
	if !strings.HasPrefix(created.ID, "IN_") || created.StreamKey == "" || !strings.HasPrefix(created.PublishURL, "rtmp://") || !created.Enabled {
		t.Errorf("created: %+v", created)
	}
	got := must[*streamkit.Ingress](t)(c.GetIngress(ctx, created.ID))
	if got.StreamKey != "" {
		t.Error("a stream key is only shown when it is made")
	}
	if got.RoomName != room || got.ParticipantIdentity != "stage" || got.ParticipantName != "Stage" || got.Name != "stage" {
		t.Errorf("got: %+v", got)
	}
	items := must[[]*streamkit.Ingress](t)(c.ListIngress(ctx))
	if !slices.ContainsFunc(items, func(in *streamkit.Ingress) bool { return in.ID == created.ID }) {
		t.Errorf("list: %+v", items)
	}

	name := "renamed"
	updated := must[*streamkit.Ingress](t)(c.UpdateIngress(ctx, created.ID, &streamkit.UpdateIngressRequest{Name: &name, Enabled: streamkit.Bool(false)}))
	if updated.Name != "renamed" || updated.Enabled {
		t.Errorf("updated: %+v", updated)
	}

	rotated := must[*streamkit.Ingress](t)(c.RotateIngressKey(ctx, created.ID))
	if rotated.StreamKey == "" || rotated.StreamKey == created.StreamKey {
		t.Errorf("rotated: %+v", rotated)
	}
	must[[]*streamkit.IngressEvent](t)(c.IngressLogs(ctx, created.ID, 10))

	whip := must[*streamkit.Ingress](t)(c.CreateIngress(ctx, &streamkit.CreateIngressRequest{InputType: streamkit.IngressWHIP, Room: room, ParticipantIdentity: "whip"}))
	if !strings.HasSuffix(whip.PublishURL, "/whip/v1") || whip.StreamKey == "" {
		t.Errorf("whip: %+v", whip)
	}

	check(t, c.DeleteIngress(ctx, created.ID))
	check(t, c.DeleteIngress(ctx, whip.ID))
	_, err := c.GetIngress(ctx, created.ID)
	wantCode(t, err, "not_found")
	_, err = c.CreateIngress(ctx, &streamkit.CreateIngressRequest{InputType: "carrier-pigeon", Room: room, ParticipantIdentity: "x"})
	wantCode(t, err, "validation_failed")
	_, err = c.CreateIngress(ctx, &streamkit.CreateIngressRequest{InputType: streamkit.IngressURL, Room: room, ParticipantIdentity: "x"})
	wantCode(t, err, "validation_failed")
	var e *streamkit.Error
	if errors.As(err, &e) && e.Fields["source_url"] == "" {
		t.Errorf("fields: %v", e.Fields)
	}
	_, err = c.RotateIngressKey(ctx, "IN_not_an_ingress")
	wantCode(t, err, "not_found")
}

// A real RTMP stream from ffmpeg, through a real worker, received by a real participant.
func TestIngressEndToEnd(t *testing.T) {
	tg := withWorkers(t)
	ctx := testContext(t)
	room := roomName(t)
	c := tg.client.Ingress
	created := must[*streamkit.Ingress](t)(c.CreateIngress(ctx, &streamkit.CreateIngressRequest{InputType: streamkit.IngressRTMP, Room: room, ParticipantIdentity: "stage"}))
	t.Cleanup(func() { _ = c.DeleteIngress(context.Background(), created.ID) })

	pushCtx, stopPush := context.WithCancel(ctx)
	defer stopPush()
	pusher := exec.CommandContext(pushCtx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-re",
		"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30", "-f", "lavfi", "-i", "sine=frequency=440:r=48000",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-g", "60", "-c:a", "aac",
		"-f", "flv", "rtmp://"+fx.Info.RTMP+"/live/"+created.StreamKey)
	pusher.Stdout, pusher.Stderr = io.Discard, os.Stderr
	check(t, pusher.Start())
	defer func() { stopPush(); _ = pusher.Wait() }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		got := must[*streamkit.Ingress](t)(c.GetIngress(ctx, created.ID))
		if got.Status == streamkit.IngressPublishing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never started publishing: %+v", got)
		}
		time.Sleep(250 * time.Millisecond)
	}

	viewer := join(t, ctx, tg, room, fixture.PeerOptions{Identity: "viewer"})
	for _, kind := range []string{"video", "audio"} {
		_, err := viewer.Wait(30*time.Second, "the stream's "+kind, func(e fixture.Event) bool {
			return e.Event == "media" && e.Kind == kind && e.Packets > 10
		})
		check(t, err)
	}
	info := must[*pb.ParticipantInfo](t)(tg.client.Room.GetParticipant(ctx, &pb.RoomParticipantIdentity{Room: room, Identity: "stage"}))
	if info.GetKind() != pb.ParticipantInfo_INGRESS {
		t.Errorf("kind: %v", info.GetKind())
	}

	must[*streamkit.Ingress](t)(c.StopIngress(ctx, created.ID))
	for {
		got := must[*streamkit.Ingress](t)(c.GetIngress(ctx, created.ID))
		if got.Status == streamkit.IngressInactive {
			break
		}
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatalf("never stopped: %+v", got)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
