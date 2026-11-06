package room

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/transport"
)

// nullSink stands in for a signalling connection. These tests look at the room's own state.
type nullSink struct{}

func (nullSink) Send(*pb.SignalResponse) error { return nil }
func (nullSink) Close(string)                  {}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.RTC.UDPPort = freePort(t)
	cfg.RTC.TCPPort = freePort(t)
	engine, err := transport.New(cfg.RTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewManager(ctx, cfg, engine, slog.New(slog.DiscardHandler))
}

func join(t *testing.T, m *Manager, roomName, identity, kind string, video auth.VideoGrant) (*participant.Participant, error) {
	t.Helper()
	video.RoomJoin = true
	video.Room = roomName
	p, err := m.Join(JoinRequest{
		RoomName: roomName,
		Verified: &auth.Verified{
			Key:    auth.Key{APIKey: "k", Secret: "s", ProjectID: "p"},
			Grants: &auth.Grants{Identity: identity, Kind: kind, Video: &video},
		},
		Client: &pb.ClientInfo{},
		Sink:   nullSink{},
	})
	if p != nil {
		t.Cleanup(func() { p.Close(false, pb.DisconnectReason_CLIENT_INITIATED) })
	}
	return p, err
}

func TestDependentsAreNotCounted(t *testing.T) {
	m := newTestManager(t)
	r, _ := m.GetOrCreate("p", "limit", CreateOptions{MaxParticipants: 1})

	agent, err := join(t, m, "limit", "agent-1", "agent", auth.VideoGrant{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !agent.IsAgent() || !agent.Dependent() {
		t.Fatal("an agent token should make an agent session")
	}
	if _, err := join(t, m, "limit", "recorder", "egress", auth.VideoGrant{Recorder: true}); err != nil {
		t.Fatalf("a recorder used up the limit: %v", err)
	}
	if r.IsFull() {
		t.Fatal("agents and recorders must not fill a room")
	}
	if _, err := join(t, m, "limit", "user", "", auth.VideoGrant{}); err != nil {
		t.Fatalf("the agent used up the limit: %v", err)
	}
	if _, err := join(t, m, "limit", "second", "", auth.VideoGrant{}); err != ErrMaxParticipants {
		t.Fatalf("got %v, want the limit enforced for the second person", err)
	}
	if _, err := join(t, m, "limit", "agent-2", "", auth.VideoGrant{Agent: true}); err != nil {
		t.Fatalf("an agent could not join a full room: %v", err)
	}
	if got := r.ToProto().GetNumParticipants(); got != 1 {
		t.Fatalf("NumParticipants = %d, want only the person counted", got)
	}
}

func TestAgentDoesNotHoldARoomOpen(t *testing.T) {
	m := newTestManager(t)
	r, _ := m.GetOrCreate("p", "alone", CreateOptions{EmptyTimeout: 100, DepartureTimeout: 10})

	agent, err := join(t, m, "alone", "agent", "agent", auth.VideoGrant{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Nobody has come yet, so an agent waiting alone closes with the empty timeout.
	if !r.idleExpired(now.Add(101 * time.Second)) {
		t.Fatal("an agent alone in a room nobody joined kept it open")
	}

	user, err := join(t, m, "alone", "user", "", auth.VideoGrant{})
	if err != nil {
		t.Fatal(err)
	}
	if r.idleExpired(now.Add(101 * time.Second)) {
		t.Fatal("a room with a person in it expired")
	}

	user.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	left := time.Now()
	if r.idleExpired(left.Add(9 * time.Second)) {
		t.Fatal("the departure timeout has not passed")
	}
	if !r.idleExpired(left.Add(11 * time.Second)) {
		t.Fatal("an agent alone after everyone left kept the room open")
	}

	// An agent leaving does not restart the wait for the people who left.
	r.mu.RLock()
	before := r.lastLeftAt
	r.mu.RUnlock()
	agent.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	r.mu.RLock()
	after := r.lastLeftAt
	r.mu.RUnlock()
	if !after.Equal(before) {
		t.Fatal("an agent leaving restarted the departure timeout")
	}
}

func TestAgentLeftHook(t *testing.T) {
	m := newTestManager(t)
	left := make(chan string, 2)
	m.SetAgentLeft(func(_ *Room, p *participant.Participant, _ pb.DisconnectReason) { left <- p.Identity() })

	user, err := join(t, m, "hook", "user", "", auth.VideoGrant{})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := join(t, m, "hook", "ghost", "agent", auth.VideoGrant{Agent: true, Hidden: true})
	if err != nil {
		t.Fatal(err)
	}
	user.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	hidden.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	select {
	case got := <-left:
		if got != "ghost" {
			t.Fatalf("hook ran for %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a hidden agent leaving was not reported")
	}
	if len(left) != 0 {
		t.Fatal("a person leaving was reported as an agent")
	}
}

func TestSenderIdentityRules(t *testing.T) {
	m := newTestManager(t)
	r, _ := m.GetOrCreate("p", "identity", CreateOptions{})
	agent, err := join(t, m, "identity", "agent", "agent", auth.VideoGrant{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	user, err := join(t, m, "identity", "user", "", auth.VideoGrant{})
	if err != nil {
		t.Fatal(err)
	}

	textHeader := func(from, stream string) *pb.DataPacket {
		return &pb.DataPacket{ParticipantIdentity: from, Value: &pb.DataPacket_StreamHeader{StreamHeader: &pb.DataStream_Header{
			StreamId: stream, ContentHeader: &pb.DataStream_Header_TextHeader{TextHeader: &pb.DataStream_TextHeader{}},
		}}}
	}

	t.Run("a person cannot speak for anyone", func(t *testing.T) {
		dp := textHeader("agent", "s1")
		if !r.stampSender(user, dp) || dp.GetParticipantIdentity() != "user" {
			t.Fatalf("identity %q", dp.GetParticipantIdentity())
		}
		if dp.GetStreamHeader().GetTextHeader().GetGenerated() {
			t.Fatal("marked as generated")
		}
	})

	t.Run("an agent's text stream keeps the identity across the stream", func(t *testing.T) {
		dp := textHeader("user", "s2")
		r.stampSender(agent, dp)
		if dp.GetParticipantIdentity() != "user" || !dp.GetStreamHeader().GetTextHeader().GetGenerated() {
			t.Fatalf("header: %q, generated %v", dp.GetParticipantIdentity(), dp.GetStreamHeader().GetTextHeader().GetGenerated())
		}
		// A chunk claims nothing, or claims the agent. Either way it must match the header.
		chunk := &pb.DataPacket{ParticipantIdentity: "agent", Value: &pb.DataPacket_StreamChunk{StreamChunk: &pb.DataStream_Chunk{StreamId: "s2"}}}
		r.stampSender(agent, chunk)
		if chunk.GetParticipantIdentity() != "user" {
			t.Fatalf("chunk identity %q", chunk.GetParticipantIdentity())
		}
		trailer := &pb.DataPacket{Value: &pb.DataPacket_StreamTrailer{StreamTrailer: &pb.DataStream_Trailer{StreamId: "s2"}}}
		r.stampSender(agent, trailer)
		if trailer.GetParticipantIdentity() != "user" {
			t.Fatalf("trailer identity %q", trailer.GetParticipantIdentity())
		}
		// The stream is over, so a chunk reusing its ID is the agent's own.
		late := &pb.DataPacket{Value: &pb.DataPacket_StreamChunk{StreamChunk: &pb.DataStream_Chunk{StreamId: "s2"}}}
		r.stampSender(agent, late)
		if late.GetParticipantIdentity() != "agent" {
			t.Fatalf("late chunk identity %q", late.GetParticipantIdentity())
		}
	})

	t.Run("a byte stream cannot speak for anyone", func(t *testing.T) {
		dp := &pb.DataPacket{ParticipantIdentity: "user", Value: &pb.DataPacket_StreamHeader{StreamHeader: &pb.DataStream_Header{
			StreamId: "s3", ContentHeader: &pb.DataStream_Header_ByteHeader{ByteHeader: &pb.DataStream_ByteHeader{}},
		}}}
		r.stampSender(agent, dp)
		if dp.GetParticipantIdentity() != "agent" {
			t.Fatalf("identity %q", dp.GetParticipantIdentity())
		}
	})

	t.Run("an agent's own stream stays its own", func(t *testing.T) {
		dp := textHeader("agent", "s4")
		r.stampSender(agent, dp)
		if dp.GetParticipantIdentity() != "agent" || dp.GetStreamHeader().GetTextHeader().GetGenerated() {
			t.Fatalf("identity %q, generated %v", dp.GetParticipantIdentity(), dp.GetStreamHeader().GetTextHeader().GetGenerated())
		}
	})

	t.Run("chat messages", func(t *testing.T) {
		dp := &pb.DataPacket{ParticipantIdentity: "user", Value: &pb.DataPacket_ChatMessage{ChatMessage: &pb.ChatMessage{Id: "1"}}}
		r.stampSender(agent, dp)
		if dp.GetParticipantIdentity() != "user" || !dp.GetChatMessage().GetGenerated() {
			t.Fatalf("from an agent: %q, generated %v", dp.GetParticipantIdentity(), dp.GetChatMessage().GetGenerated())
		}
		dp = &pb.DataPacket{ParticipantIdentity: "agent", Value: &pb.DataPacket_ChatMessage{ChatMessage: &pb.ChatMessage{Id: "2"}}}
		r.stampSender(user, dp)
		if dp.GetParticipantIdentity() != "user" || dp.GetChatMessage().GetGenerated() {
			t.Fatalf("from a person: %q, generated %v", dp.GetParticipantIdentity(), dp.GetChatMessage().GetGenerated())
		}
	})

	t.Run("only agents send transcriptions", func(t *testing.T) {
		dp := &pb.DataPacket{Value: &pb.DataPacket_Transcription{Transcription: &pb.Transcription{}}}
		if r.stampSender(user, dp) {
			t.Fatal("forwarded a transcription from a person")
		}
		if !r.stampSender(agent, dp) {
			t.Fatal("dropped a transcription from an agent")
		}
	})
}

func TestTokenRoomConfig(t *testing.T) {
	m := newTestManager(t)
	cfg := &pb.RoomConfiguration{MaxParticipants: 7, Metadata: "from token", Agents: []*pb.RoomAgentDispatch{{AgentName: "support", Metadata: "m"}}}
	g := &auth.Grants{Identity: "user", RoomConfig: cfg, Video: &auth.VideoGrant{RoomJoin: true, Room: "by-token"}}

	// It has to survive signing, parsing and cloning.
	key := auth.Key{APIKey: "k", Secret: "a-long-enough-secret-for-testing-0001", ProjectID: "p"}
	raw, err := auth.Mint(key, g, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.Verify(raw, auth.StaticKeys{ProjectID: "p", Keys: map[string]string{"k": key.Secret}})
	if err != nil {
		t.Fatal(err)
	}
	opts := m.optionsFromGrants(v.Grants.Clone())
	if opts.MaxParticipants != 7 || opts.Metadata != "from token" || len(opts.Agents) != 1 || opts.Agents[0].GetAgentName() != "support" {
		t.Fatalf("wrong options: %+v", opts)
	}

	p, err := m.Join(JoinRequest{RoomName: "by-token", Verified: &auth.Verified{Key: key, Grants: v.Grants}, Client: &pb.ClientInfo{}, Sink: nullSink{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(false, pb.DisconnectReason_CLIENT_INITIATED) })
	r := m.Get("p", "by-token")
	if got := r.ConfiguredAgents(); len(got) != 1 || got[0].GetAgentName() != "support" {
		t.Fatalf("configured agents: %v", got)
	}
	if r.ToProto().GetMaxParticipants() != 7 {
		t.Fatalf("max participants: %d", r.ToProto().GetMaxParticipants())
	}

	// A room that already exists keeps what it has.
	g2 := &auth.Grants{Identity: "user2", RoomConfig: &pb.RoomConfiguration{Agents: []*pb.RoomAgentDispatch{{AgentName: "other"}}}, Video: &auth.VideoGrant{RoomJoin: true, Room: "by-token"}}
	p2, err := m.Join(JoinRequest{RoomName: "by-token", Verified: &auth.Verified{Key: key, Grants: g2}, Client: &pb.ClientInfo{}, Sink: nullSink{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p2.Close(false, pb.DisconnectReason_CLIENT_INITIATED) })
	if got := r.ConfiguredAgents(); len(got) != 1 || got[0].GetAgentName() != "support" {
		t.Fatalf("configured agents changed: %v", got)
	}

	if got := m.optionsFromGrants(&auth.Grants{}); got.Agents != nil {
		t.Fatalf("no config gave %+v", got)
	}
}
