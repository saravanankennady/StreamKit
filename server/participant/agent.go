package participant

import (
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/track"
)

// IsAgent reports whether this session belongs to an agent, by its token's kind or its agent grant.
func (p *Participant) IsAgent() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return kindFromGrants(p.params.Grants) == pb.ParticipantInfo_AGENT || p.permission.GetAgent()
}

// Dependent reports whether this session exists only because of the people in the room: an agent or a recorder.
// A dependent does not keep a room open, does not use up its participant limit, is not counted in its participant
// totals, and does not cause agents to be dispatched.
func (p *Participant) Dependent() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch kindFromGrants(p.params.Grants) {
	case pb.ParticipantInfo_AGENT, pb.ParticipantInfo_EGRESS:
		return true
	}
	return p.permission.GetAgent() || p.permission.GetRecorder()
}

// trackBound runs when this participant's downtrack for t is bound on a connected transport. It tells the publisher
// that someone can now receive the track, once per track. It cannot wait for a forwarded packet, because a publisher
// may hold its audio until it hears this: an agent does, and neither side would ever move. Hidden subscribers do not
// count, since a recorder alone is not an audience.
func (p *Participant) trackBound(t *track.Published) {
	if p.Hidden() {
		return
	}
	_, owner := p.params.Listener.FindTrack(t.SID())
	if owner == nil || !owner.supportsTrackSubscribed() || !t.MarkSubscribedNotified() {
		return
	}
	owner.Send(&pb.SignalResponse{Message: &pb.SignalResponse_TrackSubscribed{
		TrackSubscribed: &pb.TrackSubscribed{TrackSid: t.SID()},
	}})
}

// supportsTrackSubscribed leaves out old Rust clients, which cannot decode this message.
func (p *Participant) supportsTrackSubscribed() bool {
	c := p.params.Client
	return c.GetSdk() != pb.ClientInfo_RUST || c.GetProtocol() >= 10
}
