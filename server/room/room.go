// Package room implements rooms: their lifecycle, membership, and the media and data routing between
// participants.
package room

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/datatrack"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/track"
)

// The client's ping schedule, in seconds. The server treats a longer silence as a lost connection.
const (
	PingIntervalSeconds = 5
	PingTimeoutSeconds  = 15

	speakerInterval   = 500 * time.Millisecond
	qualityInterval   = 5 * time.Second
	roomUpdateMinimum = 5 * time.Second
)

// State is a room's lifecycle stage.
type State string

// Room states. A room goes Creating, then Active and Empty as people come and go, then Closing and
// Closed. Failed is a room that could not start.
const (
	StateCreating State = "creating"
	StateActive   State = "active"
	StateEmpty    State = "empty"
	StateClosing  State = "closing"
	StateClosed   State = "closed"
	StateFailed   State = "failed"
)

// Errors joining a room. The text reaches clients, and some match the text clients look for.
var (
	ErrRoomNotFound        = errors.New("requested room does not exist")
	ErrRoomClosed          = errors.New("room is closed")
	ErrMaxParticipants     = errors.New("max participants exceeded")
	ErrParticipantNotFound = errors.New("participant does not exist")
	ErrTrackNotFound       = errors.New("track is not found")
)

// IsFull reports whether a new, visible participant would exceed the room's limit.
func (r *Room) IsFull() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	limit := r.info.GetMaxParticipants()
	if limit == 0 {
		return false
	}
	var count uint32
	for _, p := range r.participants {
		if !p.Hidden() && !p.Dependent() {
			count++
		}
	}
	return count >= limit
}

// Room is one conversation.
type Room struct {
	mgr  *Manager
	log  *slog.Logger
	proj string
	// syncStreams is set when the room is created, and does not change.
	syncStreams bool

	mu           sync.RWMutex
	info         *pb.Room
	participants map[string]*participant.Participant
	state        State
	everJoined   bool
	lastLeftAt   time.Time
	dirty        bool

	speakers map[string]float32
	quality  map[string]*qualityTracker
	stop     chan struct{}
	stopOnce sync.Once

	agents       []*pb.RoomAgentDispatch
	agentStreams agentStreams
}

func newRoom(mgr *Manager, projectID string, info *pb.Room) *Room {
	r := &Room{
		mgr:          mgr,
		proj:         projectID,
		log:          mgr.log.With("room", info.GetName(), "rID", info.GetSid()),
		info:         info,
		participants: map[string]*participant.Participant{},
		state:        StateCreating,
		speakers:     map[string]float32{},
		quality:      map[string]*qualityTracker{},
		stop:         make(chan struct{}),
	}
	go r.speakerLoop()
	go r.periodicLoop()
	return r
}

// Name returns the room's name.
func (r *Room) Name() string { return r.info.GetName() }

// SID returns the room's server-assigned ID.
func (r *Room) SID() string { return r.info.GetSid() }

// ProjectID returns the owning project.
func (r *Room) ProjectID() string { return r.proj }

// State returns the lifecycle stage.
func (r *Room) State() State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// ToProto returns the room as clients see it.
func (r *Room) ToProto() *pb.Room {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.toProtoLocked()
}

func (r *Room) toProtoLocked() *pb.Room {
	info := proto.Clone(r.info).(*pb.Room)
	var count, publishers uint32
	for _, p := range r.participants {
		if p.Hidden() {
			continue
		}
		if !p.Dependent() {
			count++
		}
		if len(p.Tracks()) > 0 {
			publishers++
		}
	}
	info.NumParticipants = count
	info.NumPublishers = publishers
	return info
}

// Participants returns the current members.
func (r *Room) Participants() []*participant.Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*participant.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p)
	}
	return out
}

// Participant returns a member by identity, or nil.
func (r *Room) Participant(identity string) *participant.Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.participants[identity]
}

func (r *Room) others(of *participant.Participant) []*participant.Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*participant.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p != of {
			out = append(out, p)
		}
	}
	return out
}

// hasMembersLocked reports whether anyone but a dependent is here. Recorders and agents do not keep a room open.
func (r *Room) hasMembersLocked() bool {
	for _, p := range r.participants {
		if !p.Dependent() {
			return true
		}
	}
	return false
}

func (r *Room) hasRecorderLocked() bool {
	for _, p := range r.participants {
		if p.Recorder() {
			return true
		}
	}
	return false
}

// join adds a participant, replacing any earlier session with the same identity.
func (r *Room) join(p *participant.Participant) error {
	r.mu.Lock()
	switch r.state {
	case StateClosing, StateClosed, StateFailed:
		r.mu.Unlock()
		return ErrRoomClosed
	}
	previous := r.participants[p.Identity()]
	limit := r.info.GetMaxParticipants()
	if limit > 0 && previous == nil && !p.Hidden() && !p.Dependent() {
		var count uint32
		for _, x := range r.participants {
			if !x.Hidden() && !x.Dependent() {
				count++
			}
		}
		if count >= limit {
			r.mu.Unlock()
			return ErrMaxParticipants
		}
	}
	r.participants[p.Identity()] = p
	// A recorder or agent is there for the people in the room. It does not count as someone being in it, so a
	// room with only those closes on the same timers as an empty one.
	if !p.Dependent() {
		r.everJoined = true
		r.state = StateActive
	}
	if p.Recorder() {
		r.info.ActiveRecording = true
	}
	r.dirty = true
	// Queue the join response before releasing the lock. Once p is in the map, other participants'
	// updates can reach it, and a client rejects a session whose first message is not join.
	p.SendJoin(r.buildJoinLocked(p))
	r.mu.Unlock()

	metrics.Participants.Inc()
	if previous != nil {
		r.log.Info("replacing session with duplicate identity", "participant", p.Identity())
		previous.Close(true, pb.DisconnectReason_DUPLICATE_IDENTITY)
	}
	p.Joined()
	go r.subscribeToExisting(p)
	return nil
}

func (r *Room) buildJoinLocked(p *participant.Participant) *pb.JoinResponse {
	info := r.toProtoLocked()
	var others []*pb.ParticipantInfo
	for _, x := range r.participants {
		if x != p && !x.Hidden() {
			others = append(others, x.ToProto())
		}
	}
	codecs := r.mgr.enabledCodecs()
	cfg := r.mgr.serverInfo()
	return &pb.JoinResponse{
		Room:                 info,
		Participant:          p.ToProto(),
		OtherParticipants:    others,
		ServerVersion:        cfg.GetVersion(),
		ServerRegion:         cfg.GetRegion(),
		IceServers:           r.mgr.iceServersFor(r.proj, p.Identity(), p.Recorder()),
		SubscriberPrimary:    p.SubscriberPrimary(),
		PingTimeout:          PingTimeoutSeconds,
		PingInterval:         PingIntervalSeconds,
		ServerInfo:           cfg,
		EnabledPublishCodecs: codecs,
		FastPublish:          p.ToProto().GetPermission().GetCanPublish(),
	}
}

func (r *Room) subscribeToExisting(p *participant.Participant) {
	for _, owner := range r.others(p) {
		for _, t := range owner.Tracks() {
			r.maybeSubscribe(p, owner, t)
		}
		if p.AutoSubscribeData() {
			for _, t := range owner.DataTrackRoutes() {
				p.SubscribeData(t.SID())
			}
		}
	}
}

// maybeSubscribe applies the subscription rules: the subscriber wants the track and its publisher allows
// it.
func (r *Room) maybeSubscribe(sub, owner *participant.Participant, t *track.Published) {
	if !sub.Wanted(t.SID()) {
		return
	}
	if !owner.AllowsSubscriber(sub, t.SID()) {
		sub.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: &pb.SubscriptionPermissionUpdate{
				ParticipantSid: owner.SID(), TrackSid: t.SID(), Allowed: false,
			},
		}})
		return
	}
	if err := sub.Subscribe(t); err != nil {
		r.log.Warn("could not subscribe", "subscriber", sub.Identity(), "track", t.SID(), "error", err)
	}
}

// OnParticipantActive implements participant.Listener. A participant counts as joined once its media
// connection is up. Signalling alone proves little: many failed sessions never get further.
func (r *Room) OnParticipantActive(p *participant.Participant) {
	// Hidden participants announce neither arrival nor, in OnClosed, departure.
	if p.Hidden() {
		return
	}
	r.emit(Event{Type: EventParticipantJoined, Participant: p.ToProto()})
}

// OnParticipantChanged implements participant.Listener: others learn of the change.
func (r *Room) OnParticipantChanged(p *participant.Participant) {
	r.broadcastInfo(p)
	r.markDirty()
}

func (r *Room) broadcastInfo(p *participant.Participant) {
	// A closed session's departure is announced once, by OnClosed, and only if it is still the current
	// session for that identity.
	if p.Hidden() || p.IsClosed() {
		return
	}
	info := p.ToProto()
	for _, other := range r.others(p) {
		other.SendParticipantUpdate([]*pb.ParticipantInfo{info})
	}
	// A participant's own view can change too, through server-side updates.
	p.SendParticipantUpdate([]*pb.ParticipantInfo{info})
}

// OnTrackPublished implements participant.Listener: the track is announced, though no media has
// arrived.
func (r *Room) OnTrackPublished(p *participant.Participant, t *track.Published) {
	r.broadcastInfo(p)
	r.markDirty()
	r.emit(Event{Type: EventTrackPublished, Participant: p.ToProto(), Track: t.Info()})
}

// OnTrackReady implements participant.Listener: media is flowing, so subscribers can be attached.
func (r *Room) OnTrackReady(p *participant.Participant, t *track.Published) {
	for _, sub := range r.others(p) {
		r.maybeSubscribe(sub, p, t)
	}
	p.SendParticipantUpdate([]*pb.ParticipantInfo{p.ToProto()})
}

// OnTrackUnpublished implements participant.Listener.
func (r *Room) OnTrackUnpublished(p *participant.Participant, t *track.Published) {
	r.broadcastInfo(p)
	r.markDirty()
	r.emit(Event{Type: EventTrackUnpublished, Participant: p.ToProto(), Track: t.Info()})
}

// OnSubscriptionRequest implements participant.Listener.
func (r *Room) OnSubscriptionRequest(p *participant.Participant, trackSIDs []string, subscribe bool) {
	for _, sid := range trackSIDs {
		p.SetWanted(sid, subscribe)
		if !subscribe {
			p.Unsubscribe(sid)
			continue
		}
		t, owner := r.FindTrack(sid)
		if t == nil || owner == p {
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionResponse{SubscriptionResponse: &pb.SubscriptionResponse{
				TrackSid: sid, Err: pb.SubscriptionError_SE_TRACK_NOTFOUND,
			}}})
			continue
		}
		r.maybeSubscribe(p, owner, t)
	}
}

// OnPermissionsChanged implements participant.Listener: a publisher changed who may subscribe.
func (r *Room) OnPermissionsChanged(owner *participant.Participant) {
	for _, sub := range r.others(owner) {
		for _, t := range owner.Tracks() {
			if owner.AllowsSubscriber(sub, t.SID()) {
				r.maybeSubscribe(sub, owner, t)
				continue
			}
			sub.Unsubscribe(t.SID())
			sub.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionPermissionUpdate{
				SubscriptionPermissionUpdate: &pb.SubscriptionPermissionUpdate{
					ParticipantSid: owner.SID(), TrackSid: t.SID(), Allowed: false,
				},
			}})
		}
		for _, t := range owner.DataTrackRoutes() {
			if !owner.AllowsSubscriber(sub, t.SID()) {
				sub.RevokeDataSubscription(t)
			}
		}
		sub.ReconcileDataSubscriptions()
	}
}

// OnDataTrackPublished implements participant.Listener. Clients ask for data tracks one at a time, so this mostly serves
// the ones whose request got here first.
func (r *Room) OnDataTrackPublished(p *participant.Participant, t *datatrack.Track) {
	for _, sub := range r.others(p) {
		if sub.AutoSubscribeData() {
			sub.SubscribeData(t.SID())
		} else {
			sub.ReconcileDataSubscriptions()
		}
	}
	r.markDirty()
}

// OnDataTrackUnpublished implements participant.Listener.
func (r *Room) OnDataTrackUnpublished(_ *participant.Participant, t *datatrack.Track) {
	for _, sub := range r.Participants() {
		sub.DataTrackEnded(t)
	}
	r.markDirty()
}

// FindDataTrack implements participant.Listener.
func (r *Room) FindDataTrack(sid string) (*datatrack.Track, *participant.Participant) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.participants {
		if t := p.DataTrack(sid); t != nil {
			return t, p
		}
	}
	return nil, nil
}

// FindTrack implements participant.Listener.
func (r *Room) FindTrack(sid string) (*track.Published, *participant.Participant) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.participants {
		if t := p.Track(sid); t != nil {
			return t, p
		}
	}
	return nil, nil
}

// OnDataPacket implements participant.Listener. The server stamps the sender's identity, which clients
// trust, so a client cannot impersonate another participant.
func (r *Room) OnDataPacket(p *participant.Participant, kind pb.DataPacket_Kind, dp *pb.DataPacket) {
	dp.Kind = kind
	if !r.stampSender(p, dp) {
		return
	}
	r.routeData(p, kind, dp)
}

// UpdateSubscriptions changes what a participant receives, on behalf of the room API.
func (r *Room) UpdateSubscriptions(p *participant.Participant, trackSIDs []string, subscribe bool) {
	r.OnSubscriptionRequest(p, trackSIDs, subscribe)
}

// SendData delivers a packet from the server side, such as from the room API.
func (r *Room) SendData(kind pb.DataPacket_Kind, dp *pb.DataPacket) {
	dp.Kind = kind
	r.routeData(nil, kind, dp)
}

func (r *Room) routeData(from *participant.Participant, kind pb.DataPacket_Kind, dp *pb.DataPacket) {
	data, err := proto.Marshal(dp)
	if err != nil {
		return
	}
	// Clients set destinations in either place, depending on the API they call.
	destinations := dp.GetDestinationIdentities()
	if user := dp.GetUser(); user != nil && len(user.GetDestinationIdentities()) > 0 {
		destinations = append(slices.Clone(destinations), user.GetDestinationIdentities()...)
	}
	for _, other := range r.others(from) {
		if len(destinations) > 0 && !slices.Contains(destinations, other.Identity()) {
			continue
		}
		other.SendData(kind, data)
	}
}

// OnClosed implements participant.Listener.
func (r *Room) OnClosed(p *participant.Participant, reason pb.DisconnectReason) {
	r.mu.Lock()
	current := r.participants[p.Identity()]
	removed := current == p
	if removed {
		delete(r.participants, p.Identity())
		if !p.Dependent() {
			r.lastLeftAt = time.Now()
		}
		r.dirty = true
		if !r.hasMembersLocked() && r.state == StateActive {
			r.state = StateEmpty
		}
		r.info.ActiveRecording = r.hasRecorderLocked()
	}
	delete(r.speakers, p.SID())
	r.mu.Unlock()
	r.agentStreams.forgetSender(p.SID())

	metrics.Participants.Dec()
	if r.mgr.agentLeft != nil && p.IsAgent() {
		r.mgr.agentLeft(r, p, reason)
	}
	if p.Hidden() {
		return
	}
	info := p.ToProto()
	info.State = pb.ParticipantInfo_DISCONNECTED
	info.DisconnectReason = reason
	// Clients match DISCONNECTED by identity, not by session. When a newer session has replaced this one,
	// announcing it would remove the new session from everyone's view.
	if removed {
		for _, other := range r.others(p) {
			other.SendParticipantUpdate([]*pb.ParticipantInfo{info})
		}
	}
	if p.WasActive() {
		r.emit(Event{Type: EventParticipantLeft, Participant: info})
	} else {
		r.emit(Event{Type: EventParticipantConnectionAborted, Participant: info})
	}
}

func (r *Room) markDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// UpdateMetadata changes the room's metadata and tells everyone.
func (r *Room) UpdateMetadata(metadata string) *pb.Room {
	r.mu.Lock()
	r.info.Metadata = metadata
	info := r.toProtoLocked()
	r.dirty = false
	r.mu.Unlock()
	r.broadcastRoom(info)
	return info
}

func (r *Room) broadcastRoom(info *pb.Room) {
	for _, p := range r.Participants() {
		p.SendRoomUpdate(info)
	}
}

// close ends the room for everyone.
func (r *Room) close(reason pb.DisconnectReason) {
	r.closeWith(reason, func(p *participant.Participant) { p.Close(true, reason) })
}

// closeWith closes the room, ending each session the given way.
func (r *Room) closeWith(reason pb.DisconnectReason, end func(*participant.Participant)) {
	r.mu.Lock()
	if r.state == StateClosing || r.state == StateClosed {
		r.mu.Unlock()
		return
	}
	r.state = StateClosing
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stop) })

	for _, p := range r.Participants() {
		end(p)
	}

	r.mu.Lock()
	r.state = StateClosed
	r.mu.Unlock()
	r.emit(Event{Type: EventRoomFinished})
	r.log.Info("room closed", "reason", reason.String())
}

// idleExpired decides whether an empty room has waited long enough to close. A room nobody has joined
// waits for its empty timeout, and one that people have left waits for its departure timeout.
func (r *Room) idleExpired(now time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.hasMembersLocked() || r.state == StateClosed || r.state == StateClosing {
		return false
	}
	if r.everJoined {
		return now.Sub(r.lastLeftAt) >= time.Duration(r.info.GetDepartureTimeout())*time.Second
	}
	created := time.UnixMilli(r.info.GetCreationTimeMs())
	return now.Sub(created) >= time.Duration(r.info.GetEmptyTimeout())*time.Second
}

func (r *Room) speakerLoop() {
	t := time.NewTicker(speakerInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.updateSpeakers()
		}
	}
}

func (r *Room) updateSpeakers() {
	current := map[string]float32{}
	for _, p := range r.Participants() {
		var best float32
		for _, t := range p.Tracks() {
			if t.Kind() != pb.TrackType_AUDIO || t.Muted() {
				continue
			}
			receiver := t.Receiver()
			if receiver == nil {
				continue
			}
			if level, active := receiver.AudioLevel(); active && level > best {
				best = level
			}
		}
		if best > 0 {
			current[p.SID()] = best
		}
	}

	r.mu.Lock()
	var changes []*pb.SpeakerInfo
	for sid, level := range current {
		if _, was := r.speakers[sid]; !was {
			changes = append(changes, &pb.SpeakerInfo{Sid: sid, Level: level, Active: true})
		}
	}
	for sid := range r.speakers {
		if _, still := current[sid]; !still {
			changes = append(changes, &pb.SpeakerInfo{Sid: sid, Active: false})
		}
	}
	r.speakers = current
	r.mu.Unlock()

	if len(changes) == 0 {
		return
	}
	for _, p := range r.Participants() {
		p.SendSpeakers(changes)
	}
}

func (r *Room) periodicLoop() {
	quality := time.NewTicker(qualityInterval)
	defer quality.Stop()
	roomTick := time.NewTicker(roomUpdateMinimum)
	defer roomTick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-quality.C:
			r.updateConnectionQuality()
		case <-roomTick.C:
			r.mu.Lock()
			dirty := r.dirty
			r.dirty = false
			info := r.toProtoLocked()
			r.mu.Unlock()
			if dirty {
				r.broadcastRoom(info)
			}
		}
	}
}

// qualityTracker remembers publisher counters between checks so loss is computed per interval.
type qualityTracker struct {
	packets uint64
	lost    uint64
}

func (r *Room) updateConnectionQuality() {
	members := r.Participants()
	r.mu.Lock()
	defer r.mu.Unlock()
	live := make(map[string]*qualityTracker, len(members))
	updates := make([]*pb.ConnectionQualityInfo, 0, len(members))
	for _, p := range members {
		s := p.Stats()
		prev := r.quality[p.SID()]
		if prev == nil {
			prev = &qualityTracker{}
			r.quality[p.SID()] = prev
		}
		deltaPackets := s.PublishPackets - prev.packets
		deltaLost := s.PublishLost - prev.lost
		prev.packets, prev.lost = s.PublishPackets, s.PublishLost
		live[p.SID()] = prev

		var publishLoss float64
		if total := deltaPackets + deltaLost; total > 0 {
			publishLoss = float64(deltaLost) / float64(total)
		}
		score, quality := ScoreConnection(max(publishLoss, float64(s.SubscribeLoss)), s.SubscribeRTT, s.SubscribeJitter)
		p.SetConnectionQuality(score, quality)
		updates = append(updates, &pb.ConnectionQualityInfo{ParticipantSid: p.SID(), Quality: quality, Score: score})
	}
	r.quality = live
	for _, p := range members {
		if len(updates) > 0 {
			p.SendConnectionQuality(updates)
		}
	}
}

// ScoreConnection turns measured network conditions into a score from 1 to 5, and its quality band. It
// is a simplified E-model: loss hurts most, delay hurts once it nears what conversation tolerates.
func ScoreConnection(loss float64, rtt, jitter time.Duration) (float32, pb.ConnectionQuality) {
	const best = 4.5
	score := best
	score -= min(loss*100*0.25, 3.5)
	delay := rtt/2 + 2*jitter
	if delay > 150*time.Millisecond {
		score -= min(float64(delay-150*time.Millisecond)/float64(100*time.Millisecond), 3)
	}
	if score < 1 {
		score = 1
	}
	switch {
	case score >= 3.8:
		return float32(score), pb.ConnectionQuality_EXCELLENT
	case score >= 2.8:
		return float32(score), pb.ConnectionQuality_GOOD
	}
	return float32(score), pb.ConnectionQuality_POOR
}

// EventType names something that happened in a room.
type EventType string

// Event types. The names match the webhook event names.
const (
	EventRoomStarted       EventType = "room_started"
	EventRoomFinished      EventType = "room_finished"
	EventParticipantJoined EventType = "participant_joined"
	EventParticipantLeft   EventType = "participant_left"
	EventTrackPublished    EventType = "track_published"
	EventTrackUnpublished  EventType = "track_unpublished"
	// EventParticipantConnectionAborted is a session that signalled but never connected its media.
	EventParticipantConnectionAborted EventType = "participant_connection_aborted"
)

// Event describes a room lifecycle change.
type Event struct {
	Type        EventType
	ProjectID   string
	Room        *pb.Room
	Participant *pb.ParticipantInfo
	Track       *pb.TrackInfo
	At          time.Time
}

func (r *Room) emit(e Event) {
	r.mu.RLock()
	room := r.toProtoLocked()
	r.mu.RUnlock()
	r.dispatch(e, room)
}

func (r *Room) dispatch(e Event, room *pb.Room) {
	e.Room = room
	e.ProjectID = r.proj
	e.At = time.Now()
	r.mgr.publishEvent(e)
}

var _ participant.Listener = (*Room)(nil)
