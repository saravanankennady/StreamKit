package room

import (
	"sync"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/participant"
)

// AgentLeftFunc is told when a session belonging to an agent ends in a room.
type AgentLeftFunc func(r *Room, p *participant.Participant, reason pb.DisconnectReason)

// SetAgentLeft installs what runs when an agent's session ends. It is separate from room events because hidden
// participants raise no events, and a worker may register with hidden agents.
func (m *Manager) SetAgentLeft(fn AgentLeftFunc) { m.agentLeft = fn }

// ConfiguredAgents returns the agents the room was created with, through a token or the room API. Empty means the
// room gets the automatic dispatch.
func (r *Room) ConfiguredAgents() []*pb.RoomAgentDispatch { return r.agents }

// optionsFromGrants turns a token's room configuration into creation options.
func (m *Manager) optionsFromGrants(g *auth.Grants) CreateOptions {
	cfg := g.RoomConfig
	return CreateOptions{
		EmptyTimeout:     cfg.GetEmptyTimeout(),
		DepartureTimeout: cfg.GetDepartureTimeout(),
		MaxParticipants:  cfg.GetMaxParticipants(),
		Metadata:         cfg.GetMetadata(),
		Agents:           cfg.GetAgents(),
	}
}

// agentStreams remembers which data streams an agent is sending on someone else's behalf, so the whole stream carries
// one identity. Clients drop stream packets whose identity differs from the stream's header.
type agentStreams struct {
	mu sync.Mutex
	// by sender session, then stream ID
	bySender map[string]map[string]string
}

func (a *agentStreams) remember(senderSID, streamID, identity string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bySender == nil {
		a.bySender = map[string]map[string]string{}
	}
	if a.bySender[senderSID] == nil {
		a.bySender[senderSID] = map[string]string{}
	}
	a.bySender[senderSID][streamID] = identity
}

func (a *agentStreams) lookup(senderSID, streamID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bySender[senderSID][streamID]
}

func (a *agentStreams) forget(senderSID, streamID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bySender[senderSID], streamID)
}

func (a *agentStreams) forgetSender(senderSID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bySender, senderSID)
}

// stampSender sets a data packet's sender identity, which clients trust, and reports whether to forward the packet.
//
// The server normally overwrites the identity, so nobody can impersonate anyone. An agent may speak for someone else
// in two cases, because that is how a user's speech reaches clients as a transcript of that user: a chat message and
// a text stream. Those are marked as generated, so clients can tell. Transcription packets are only forwarded from
// agents.
func (r *Room) stampSender(p *participant.Participant, dp *pb.DataPacket) bool {
	claimed := dp.GetParticipantIdentity()
	onBehalf := claimed != "" && claimed != p.Identity() && p.IsAgent()
	keep := false
	switch v := dp.GetValue().(type) {
	case *pb.DataPacket_Transcription:
		if !p.IsAgent() {
			return false
		}
	case *pb.DataPacket_ChatMessage:
		if onBehalf && v.ChatMessage != nil {
			v.ChatMessage.Generated = true
			keep = true
		}
	case *pb.DataPacket_StreamHeader:
		text := v.StreamHeader.GetTextHeader()
		if onBehalf && text != nil {
			text.Generated = true
			keep = true
			r.agentStreams.remember(p.SID(), v.StreamHeader.GetStreamId(), claimed)
		}
	case *pb.DataPacket_StreamChunk:
		if identity := r.agentStreams.lookup(p.SID(), v.StreamChunk.GetStreamId()); identity != "" {
			dp.ParticipantIdentity = identity
			keep = true
		}
	case *pb.DataPacket_StreamTrailer:
		if identity := r.agentStreams.lookup(p.SID(), v.StreamTrailer.GetStreamId()); identity != "" {
			dp.ParticipantIdentity = identity
			keep = true
			r.agentStreams.forget(p.SID(), v.StreamTrailer.GetStreamId())
		}
	}
	if !keep {
		dp.ParticipantIdentity = p.Identity()
	}
	dp.ParticipantSid = p.SID()
	if user := dp.GetUser(); user != nil {
		user.ParticipantIdentity = p.Identity()
		user.ParticipantSid = p.SID()
	}
	return true
}

// ServerInfo returns this server as clients and workers see it.
func (m *Manager) ServerInfo() *pb.ServerInfo { return m.serverInfo() }

// AutoCreateEnabled reports whether a project's rooms can be created by naming them.
func (m *Manager) AutoCreateEnabled(projectID string) bool {
	if v := m.ProjectConfigFor(projectID).AutoCreate; v != nil {
		return *v
	}
	return m.cfg.Room.AutoCreate
}
