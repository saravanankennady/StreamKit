package participant

import (
	"math/rand/v2"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/datatrack"
)

const (
	maxDataTrackHandle = 65535
	maxDataTrackName   = 256
	// maxDataTrackEncoding limits a custom encoding or schema name.
	maxDataTrackEncoding = 32
	// maxDataTracks limits how many data tracks one participant can publish. The reference sets no limit. This is
	// client-controlled traffic fanned out to everyone subscribed, so there has to be one.
	maxDataTracks = 64
	// dataTrackBuffer is how much can wait to go out on a data track channel. Late packets have no value here, and a
	// subscriber that falls behind by this much is better off losing some than delaying the rest.
	dataTrackBuffer = 1 << 20
	// How long a subscription to a track that does not exist can wait for it. A client gives up after 10 s.
	dataTrackNotFoundTimeout = 10 * time.Second
)

// dataSubscription is a track this participant asked to receive.
type dataSubscription struct {
	sid     string
	askedAt time.Time
	// track and handle are set while it is being received.
	track  *datatrack.Track
	handle uint16
	// denied says the client has been told the publisher does not allow this.
	denied bool
}

func (p *Participant) sendRequestResponse(rr *pb.RequestResponse) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_RequestResponse{RequestResponse: rr}})
}

// dataEncodingOK checks the names of custom encodings. Well-known ones have no name to get wrong.
func dataEncodingOK(req *pb.PublishDataTrackRequest) bool {
	ok := func(name string) bool { return name != "" && len(name) <= maxDataTrackEncoding }
	if c, custom := req.GetFrameEncoding().GetValue().(*pb.DataTrackFrameEncoding_Custom); custom && !ok(c.Custom) {
		return false
	}
	if c, custom := req.GetSchema().GetEncoding().GetValue().(*pb.DataTrackSchemaEncoding_Custom); custom && !ok(c.Custom) {
		return false
	}
	return true
}

// handlePublishDataTrack answers a publish request with what is wrong with it, in the order the reference checks, or with
// the new track. A client waiting for its answer finds the request by the handle in the echoed copy.
func (p *Participant) handlePublishDataTrack(req *pb.PublishDataTrackRequest) {
	refuse := func(reason pb.RequestResponse_Reason, message string) {
		p.sendRequestResponse(&pb.RequestResponse{
			Reason:  reason,
			Message: message,
			Request: &pb.RequestResponse_PublishDataTrack{PublishDataTrack: proto.Clone(req).(*pb.PublishDataTrackRequest)},
		})
	}
	p.mu.RLock()
	allowed := p.permission.GetCanPublishData()
	p.mu.RUnlock()
	if !allowed {
		refuse(pb.RequestResponse_NOT_ALLOWED, "does not have permission to publish data")
		return
	}
	if req.GetPubHandle() == 0 || req.GetPubHandle() > maxDataTrackHandle {
		refuse(pb.RequestResponse_INVALID_HANDLE, "handle should be > 0 AND < 65536")
		return
	}
	if len(req.GetName()) == 0 || len(req.GetName()) > maxDataTrackName {
		refuse(pb.RequestResponse_INVALID_NAME, "name should not be empty and should not exceed 256 characters")
		return
	}
	if !dataEncodingOK(req) {
		refuse(pb.RequestResponse_INVALID_REQUEST, "encoding identifier is empty or exceeds the maximum length")
		return
	}

	info := &pb.DataTrackInfo{
		PubHandle:  req.GetPubHandle(),
		Sid:        NewID(DataTrackPrefix),
		Name:       req.GetName(),
		Encryption: req.GetEncryption(),
	}
	if e := req.GetFrameEncoding(); e != nil {
		info.FrameEncoding = proto.Clone(e).(*pb.DataTrackFrameEncoding)
	}
	if s := req.GetSchema(); s != nil {
		info.Schema = proto.Clone(s).(*pb.DataTrackSchemaId)
	}
	t := datatrack.NewTrack(info.GetSid(), info.GetName(), uint16(info.GetPubHandle()), p.sid, p.params.Identity)

	p.mu.Lock()
	var duplicate *pb.DataTrackInfo
	var reason pb.RequestResponse_Reason
	var message string
	for _, existing := range p.dataTracks {
		switch {
		case existing.GetPubHandle() == req.GetPubHandle():
			duplicate, reason, message = existing, pb.RequestResponse_DUPLICATE_HANDLE, "a data track with same handle already exists"
		case existing.GetName() == req.GetName():
			duplicate, reason, message = existing, pb.RequestResponse_DUPLICATE_NAME, "a data track with same name already exists"
		}
		if duplicate != nil {
			break
		}
	}
	if duplicate != nil {
		p.mu.Unlock()
		refuse(reason, message)
		return
	}
	if len(p.dataTracks) >= maxDataTracks {
		p.mu.Unlock()
		refuse(pb.RequestResponse_LIMIT_EXCEEDED, "too many data tracks")
		return
	}
	p.dataTracks[uint16(info.GetPubHandle())] = info
	p.dataTrackRoutes[uint16(info.GetPubHandle())] = t
	p.mu.Unlock()

	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_PublishDataTrackResponse{
		PublishDataTrackResponse: &pb.PublishDataTrackResponse{Info: proto.Clone(info).(*pb.DataTrackInfo)},
	}})
	p.changed()
	p.params.Listener.OnDataTrackPublished(p, t)
}

func (p *Participant) handleUnpublishDataTrack(req *pb.UnpublishDataTrackRequest) {
	handle := req.GetPubHandle()
	p.mu.Lock()
	var info *pb.DataTrackInfo
	var t *datatrack.Track
	if handle > 0 && handle <= maxDataTrackHandle {
		info = p.dataTracks[uint16(handle)]
		t = p.dataTrackRoutes[uint16(handle)]
		delete(p.dataTracks, uint16(handle))
		delete(p.dataTrackRoutes, uint16(handle))
	}
	p.mu.Unlock()
	if info == nil {
		p.sendRequestResponse(&pb.RequestResponse{
			Reason:  pb.RequestResponse_NOT_FOUND,
			Request: &pb.RequestResponse_UnpublishDataTrack{UnpublishDataTrack: proto.Clone(req).(*pb.UnpublishDataTrackRequest)},
		})
		return
	}
	p.endDataTrack(t)
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_UnpublishDataTrackResponse{
		UnpublishDataTrackResponse: &pb.UnpublishDataTrackResponse{Info: info},
	}})
	p.changed()
}

// endDataTrack tells everyone receiving a track that it has gone.
func (p *Participant) endDataTrack(t *datatrack.Track) {
	p.params.Listener.OnDataTrackUnpublished(p, t)
}

// DataTrackEnded forgets a subscription to a track that no longer exists, and says so.
func (p *Participant) DataTrackEnded(t *datatrack.Track) {
	p.mu.Lock()
	sub := p.dataSubs[t.SID()]
	if sub != nil {
		delete(p.dataSubs, t.SID())
	}
	p.mu.Unlock()
	if sub != nil && sub.track != nil {
		p.sendDataHandles()
	}
}

// handleDataSubscription applies a client's requests to start and stop receiving tracks.
func (p *Participant) handleDataSubscription(u *pb.UpdateDataSubscription) {
	changed := false
	for _, update := range u.GetUpdates() {
		sid := update.GetTrackSid()
		if update.GetSubscribe() {
			p.SubscribeData(sid)
			continue
		}
		p.mu.Lock()
		sub := p.dataSubs[sid]
		delete(p.dataSubs, sid)
		p.mu.Unlock()
		if sub != nil && sub.track != nil {
			sub.track.Remove(p.sid)
			changed = true
		}
	}
	if changed {
		p.sendDataHandles()
	}
}

// SubscribeData asks for a track, which may not exist yet: a subscribe can overtake the publish it refers to.
func (p *Participant) SubscribeData(sid string) {
	p.mu.Lock()
	sub := p.dataSubs[sid]
	if sub == nil {
		sub = &dataSubscription{sid: sid, askedAt: time.Now()}
		p.dataSubs[sid] = sub
	}
	resolved := sub.track != nil
	p.mu.Unlock()
	if resolved {
		// A repeat is answered with the current map, in case the last one was lost.
		p.sendDataHandles()
		return
	}
	p.ReconcileDataSubscriptions()
}

// ReconcileDataSubscriptions tries every subscription still waiting on its track or on permission. It runs whenever either
// could have changed. Each request is retried until it resolves, and only a track that has not turned up gives up.
func (p *Participant) ReconcileDataSubscriptions() {
	p.mu.Lock()
	canSubscribe := p.permission.GetCanSubscribe()
	waiting := make([]*dataSubscription, 0, len(p.dataSubs))
	for _, sub := range p.dataSubs {
		if sub.track == nil {
			waiting = append(waiting, sub)
		}
	}
	p.mu.Unlock()
	if !canSubscribe {
		return
	}
	now := time.Now()
	changed := false
	for _, sub := range waiting {
		t, owner := p.params.Listener.FindDataTrack(sub.sid)
		if t == nil {
			if now.Sub(sub.askedAt) > dataTrackNotFoundTimeout {
				p.mu.Lock()
				if p.dataSubs[sub.sid] == sub {
					delete(p.dataSubs, sub.sid)
				}
				p.mu.Unlock()
			}
			continue
		}
		allowed := owner.AllowsSubscriber(p, t.SID())
		p.mu.Lock()
		if p.dataSubs[sub.sid] != sub || sub.track != nil {
			p.mu.Unlock()
			continue
		}
		tell := sub.denied == allowed
		sub.denied = !allowed
		var handle uint16
		if allowed {
			handle = p.nextDataHandleLocked()
			sub.track, sub.handle = t, handle
		}
		p.mu.Unlock()
		if tell {
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionPermissionUpdate{
				SubscriptionPermissionUpdate: &pb.SubscriptionPermissionUpdate{ParticipantSid: owner.SID(), TrackSid: t.SID(), Allowed: allowed},
			}})
		}
		if allowed {
			t.Add(&datatrack.Subscriber{ID: p.sid, Handle: handle, Send: p.dataTrack.sendLossy})
			changed = true
		}
	}
	if changed {
		p.sendDataHandles()
	}
}

// nextDataHandleLocked gives each subscription its own handle, which never means the same as the publisher's.
func (p *Participant) nextDataHandleLocked() uint16 {
	if p.nextDataHandle == 0 {
		p.nextDataHandle = uint16(rand.N(255)) + 1
	}
	for {
		p.nextDataHandle++
		if p.nextDataHandle == 0 {
			continue
		}
		used := false
		for _, sub := range p.dataSubs {
			if sub.track != nil && sub.handle == p.nextDataHandle {
				used = true
				break
			}
		}
		if !used {
			return p.nextDataHandle
		}
	}
}

// sendDataHandles sends the complete map of what this participant receives. The client has to have it before a packet
// arrives under a new handle, so this goes first on the signalling connection, and packets can only lag it.
func (p *Participant) sendDataHandles() {
	handles := map[uint32]*pb.DataTrackSubscriberHandles_PublishedDataTrack{}
	p.mu.RLock()
	for _, sub := range p.dataSubs {
		if sub.track == nil {
			continue
		}
		handles[uint32(sub.handle)] = &pb.DataTrackSubscriberHandles_PublishedDataTrack{
			PublisherIdentity: sub.track.OwnerIdentity(),
			PublisherSid:      sub.track.OwnerSID(),
			TrackSid:          sub.track.SID(),
		}
	}
	p.mu.RUnlock()
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_DataTrackSubscriberHandles{
		DataTrackSubscriberHandles: &pb.DataTrackSubscriberHandles{SubHandles: handles},
	}})
}

// onDataTrackMessage takes a packet from the publisher. Anything malformed, or for a track that does not exist, is dropped
// with nothing said: this is a lossy stream, and a client would get no use from being told.
func (p *Participant) onDataTrackMessage(data []byte) {
	if limit := p.params.MaxDataPacketBytes; limit > 0 && len(data) > limit {
		return
	}
	h, err := datatrack.Parse(data)
	if err != nil {
		p.log.Debug("bad data track packet", "error", err)
		return
	}
	p.mu.RLock()
	t := p.dataTrackRoutes[h.Handle]
	allowed := p.permission.GetCanPublishData()
	p.mu.RUnlock()
	if t != nil && allowed {
		t.Forward(data)
	}
}

// DataTrack returns one of this participant's published data tracks by ID, or nil.
func (p *Participant) DataTrack(sid string) *datatrack.Track {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, t := range p.dataTrackRoutes {
		if t.SID() == sid {
			return t
		}
	}
	return nil
}

// DataTrackRoutes returns this participant's published data tracks.
func (p *Participant) DataTrackRoutes() []*datatrack.Track {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*datatrack.Track, 0, len(p.dataTrackRoutes))
	for _, t := range p.dataTrackRoutes {
		out = append(out, t)
	}
	return out
}

// AutoSubscribeData reports whether this participant receives new data tracks without asking.
func (p *Participant) AutoSubscribeData() bool { return p.params.AutoSubscribeData }

// RevokeDataSubscription stops a subscriber receiving a track whose publisher no longer allows it. It stays wanted, so that
// it comes back if the publisher changes their mind.
func (p *Participant) RevokeDataSubscription(t *datatrack.Track) {
	p.mu.Lock()
	sub := p.dataSubs[t.SID()]
	active := sub != nil && sub.track == t
	if active {
		sub.track, sub.handle = nil, 0
		sub.denied = true
	}
	p.mu.Unlock()
	if !active {
		return
	}
	t.Remove(p.sid)
	p.sendDataHandles()
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionPermissionUpdate{
		SubscriptionPermissionUpdate: &pb.SubscriptionPermissionUpdate{ParticipantSid: t.OwnerSID(), TrackSid: t.SID(), Allowed: false},
	}})
}

// revokeAllDataSubscriptions is for this participant losing permission to subscribe.
func (p *Participant) revokeAllDataSubscriptions() {
	p.mu.Lock()
	var removed []*datatrack.Track
	for _, sub := range p.dataSubs {
		if sub.track != nil {
			removed = append(removed, sub.track)
			sub.track, sub.handle = nil, 0
		}
	}
	p.mu.Unlock()
	for _, t := range removed {
		t.Remove(p.sid)
	}
	if len(removed) > 0 {
		p.sendDataHandles()
	}
}

// unpublishAllDataTracks is for this participant leaving, or losing permission to publish data.
func (p *Participant) unpublishAllDataTracks(tellClient bool) {
	p.mu.Lock()
	infos := make(map[uint16]*pb.DataTrackInfo, len(p.dataTracks))
	routes := p.dataTrackRoutes
	for h, info := range p.dataTracks {
		infos[h] = info
	}
	p.dataTracks = map[uint16]*pb.DataTrackInfo{}
	p.dataTrackRoutes = map[uint16]*datatrack.Track{}
	p.mu.Unlock()
	for h, t := range routes {
		p.endDataTrack(t)
		if tellClient {
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_UnpublishDataTrackResponse{
				UnpublishDataTrackResponse: &pb.UnpublishDataTrackResponse{Info: infos[h]},
			}})
		}
	}
}

// leaveDataSubscriptions stops receiving everything, for a session that is ending.
func (p *Participant) leaveDataSubscriptions() {
	p.mu.Lock()
	subs := p.dataSubs
	p.dataSubs = map[string]*dataSubscription{}
	p.mu.Unlock()
	for _, sub := range subs {
		if sub.track != nil {
			sub.track.Remove(p.sid)
		}
	}
}

// missingDataTrack returns the first track a resuming client claims to publish that this server does not have. A client in
// that state cannot be repaired by a resume: it has to start again.
func (p *Participant) missingDataTrack(claims []*pb.PublishDataTrackResponse) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, claim := range claims {
		info := claim.GetInfo()
		if info == nil {
			return "", true
		}
		found := false
		for _, t := range p.dataTrackRoutes {
			if t.SID() == info.GetSid() {
				found = true
				break
			}
		}
		if !found {
			return info.GetSid(), true
		}
	}
	return "", false
}
