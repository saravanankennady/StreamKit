package room

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/transport"
)

// ProtocolVersion is the signalling protocol revision this server speaks.
const ProtocolVersion = 16

const maintenanceInterval = time.Second

// roomKey scopes room names to a project, so tenants cannot collide or see each other's rooms.
type roomKey struct {
	project string
	name    string
}

// Manager owns all rooms on this node.
type Manager struct {
	cfg    *config.Config
	engine *transport.Engine
	log    *slog.Logger
	nodeID string

	mu    sync.RWMutex
	rooms map[roomKey]*Room

	listenersMu sync.RWMutex
	listeners   []func(Event)

	projectConfig func(projectID string) ProjectConfig
	admission     Admission
	draining      atomic.Bool
	agentLeft     AgentLeftFunc
}

// Admission is the control plane's say in who may start a session. A valid token proves who someone is, and it can be
// hours old: this is where what has happened since counts, such as a host removing them.
type Admission interface {
	// Admit returns an error if these grants may not start a session here, and how to open the room if this opens it.
	// Without commit it is only a question, and changes nothing. With it, the grants may be updated.
	Admit(projectID, room string, grants *auth.Grants, commit bool) (Admitted, error)
	// Removed hears of someone being removed, before they can hear of it themselves.
	Removed(projectID, room, identity string)
}

// Admitted says how a room should start, if the session admission let in is the one to open it.
type Admitted struct {
	Metadata string
	// MaxParticipants of zero says nothing about it.
	MaxParticipants uint32
}

// RefusedError is a session the admission check turned down. Its text reaches the client.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// SetAdmission installs the admission check. Without one, a valid token is enough.
func (m *Manager) SetAdmission(a Admission) { m.admission = a }

// CheckAdmission asks whether a session would be admitted, without starting one.
func (m *Manager) CheckAdmission(projectID, room string, grants *auth.Grants) error {
	if m.admission == nil {
		return nil
	}
	_, err := m.admission.Admit(projectID, room, grants, false)
	return err
}

// Remove keeps an identity out for good, and reports whether it had a session here to close. Admission hears whether or
// not it does, and first, so the token cannot come straight back.
func (m *Manager) Remove(projectID, roomName, identity string) bool {
	if m.admission != nil {
		m.admission.Removed(projectID, roomName, identity)
	}
	room := m.Get(projectID, roomName)
	if room == nil {
		return false
	}
	p := room.Participant(identity)
	if p == nil {
		return false
	}
	p.Close(true, pb.DisconnectReason_PARTICIPANT_REMOVED)
	return true
}

// NewManager creates a Manager and starts its maintenance loop, which runs until ctx ends.
func NewManager(ctx context.Context, cfg *config.Config, engine *transport.Engine, log *slog.Logger) *Manager {
	m := &Manager{
		cfg:    cfg,
		engine: engine,
		log:    log,
		nodeID: participant.NewID("ND_"),
		rooms:  map[roomKey]*Room{},
	}
	go m.maintain(ctx)
	return m
}

// NodeID returns this node's identifier.
func (m *Manager) NodeID() string { return m.nodeID }

// Subscribe registers a function called for every room event. It runs on the caller's goroutine and must
// return quickly.
func (m *Manager) Subscribe(fn func(Event)) {
	m.listenersMu.Lock()
	defer m.listenersMu.Unlock()
	m.listeners = append(m.listeners, fn)
}

func (m *Manager) publishEvent(e Event) {
	m.listenersMu.RLock()
	defer m.listenersMu.RUnlock()
	for _, fn := range m.listeners {
		fn(e)
	}
}

func (m *Manager) serverInfo() *pb.ServerInfo {
	return &pb.ServerInfo{
		Edition:   pb.ServerInfo_Standard,
		Version:   config.WireCompatVersion,
		Protocol:  ProtocolVersion,
		Region:    m.cfg.Region,
		NodeId:    m.nodeID,
		DebugInfo: "elnova-streamkit/" + config.Version,
	}
}

// iceServersFor returns what a client should use: the project's own servers if it set any, else the operator's, along with
// credentials for the operator's TURN server. A recorder only ever gets the operator's. It runs on the operator's network,
// where an address chosen by a project could reach things it should not.
func (m *Manager) iceServersFor(projectID, identity string, recorder bool) []*pb.ICEServer {
	var servers []*pb.ICEServer
	if !recorder {
		servers = m.ProjectConfigFor(projectID).ICEServers
	}
	if len(servers) == 0 {
		servers = m.iceServers()
	}
	if m.cfg.RTC.TURN.Secret != "" && len(m.cfg.RTC.TURN.URLs) > 0 {
		servers = append(slices.Clone(servers), mintTURN(m.cfg.RTC.TURN, identity, time.Now()))
	}
	return servers
}

func (m *Manager) iceServers() []*pb.ICEServer {
	out := make([]*pb.ICEServer, 0, len(m.cfg.RTC.ICEServers))
	for _, s := range m.cfg.RTC.ICEServers {
		out = append(out, &pb.ICEServer{Urls: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

func (m *Manager) enabledCodecs() []*pb.Codec {
	names := transport.EnabledPublishCodecs()
	out := make([]*pb.Codec, 0, len(names))
	for _, n := range names {
		out = append(out, &pb.Codec{Mime: n})
	}
	return out
}

func enabledCodecsForRoom() []*pb.Codec {
	names := append(transport.EnabledPublishCodecs(), webrtc.MimeTypeRTX)
	out := make([]*pb.Codec, 0, len(names))
	for _, n := range names {
		out = append(out, &pb.Codec{Mime: n})
	}
	return out
}

// ProjectConfig is what a project's settings change about its rooms.
type ProjectConfig struct {
	EmptyTimeout     uint32
	DepartureTimeout uint32
	MaxParticipants  uint32
	// AutoCreate, when set, overrides the server's default for rooms created by joining.
	AutoCreate *bool
	// AllowedOrigins, when not empty, limits which web origins may connect.
	AllowedOrigins []string
	ICEServers     []*pb.ICEServer
}

// SetProjectConfig installs the function that looks up a project's settings. Without one, every project
// uses the server defaults.
func (m *Manager) SetProjectConfig(fn func(projectID string) ProjectConfig) { m.projectConfig = fn }

// ProjectConfigFor returns a project's settings.
func (m *Manager) ProjectConfigFor(projectID string) ProjectConfig {
	if m.projectConfig == nil {
		return ProjectConfig{}
	}
	return m.projectConfig(projectID)
}

// SetDraining stops this node from accepting new sessions, ahead of maintenance. Existing rooms carry on.
func (m *Manager) SetDraining(v bool) { m.draining.Store(v) }

// Draining reports whether this node is refusing new sessions.
func (m *Manager) Draining() bool { return m.draining.Load() }

// CreateOptions customizes a new room. Zero values use the server defaults.
type CreateOptions struct {
	EmptyTimeout     uint32
	DepartureTimeout uint32
	MaxParticipants  uint32
	Metadata         string
	// Agents are dispatched to the room when it is created, in place of the automatic dispatch.
	Agents []*pb.RoomAgentDispatch
	// SyncStreams turns on sync_streams for this room, whatever the server's default. See config.Room.
	SyncStreams bool
}

// GetOrCreate returns the named room, creating it if needed. created reports which happened.
func (m *Manager) GetOrCreate(projectID, name string, opts CreateOptions) (r *Room, created bool) {
	key := roomKey{projectID, name}
	project := m.ProjectConfigFor(projectID)
	opts.EmptyTimeout = pick(opts.EmptyTimeout, project.EmptyTimeout)
	opts.DepartureTimeout = pick(opts.DepartureTimeout, project.DepartureTimeout)
	opts.MaxParticipants = pick(opts.MaxParticipants, project.MaxParticipants)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.rooms[key]; existing != nil && existing.State() != StateClosed && existing.State() != StateClosing {
		return existing, false
	}
	now := time.Now()
	info := &pb.Room{
		Sid:              participant.NewID(participant.RoomPrefix),
		Name:             name,
		EmptyTimeout:     pick(opts.EmptyTimeout, m.cfg.Room.EmptyTimeout),
		DepartureTimeout: pick(opts.DepartureTimeout, m.cfg.Room.DepartureTimeout),
		MaxParticipants:  pick(opts.MaxParticipants, m.cfg.Room.MaxParticipants),
		CreationTime:     now.Unix(),
		CreationTimeMs:   now.UnixMilli(),
		Metadata:         opts.Metadata,
		EnabledCodecs:    enabledCodecsForRoom(),
		Version:          &pb.TimedVersion{UnixMicro: now.UnixMicro()},
	}
	room := newRoom(m, projectID, info)
	room.agents = opts.Agents
	room.syncStreams = opts.SyncStreams || m.cfg.Room.SyncStreams
	m.rooms[key] = room
	metrics.Rooms.Inc()
	m.log.Info("room created", "room", name, "project", projectID, "rID", info.GetSid())
	// Listeners run outside the manager's lock, because they may call back into it.
	go room.emit(Event{Type: EventRoomStarted})
	return room, true
}

func pick(v, def uint32) uint32 {
	if v != 0 {
		return v
	}
	return def
}

// Get returns a room, or nil.
func (m *Manager) Get(projectID, name string) *Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rooms[roomKey{projectID, name}]
}

// List returns a project's rooms, narrowed to names when any are given.
func (m *Manager) List(projectID string, names []string) []*Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Room
	for k, r := range m.rooms {
		if k.project != projectID {
			continue
		}
		if len(names) > 0 {
			found := false
			for _, n := range names {
				if n == k.name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// AllRooms returns every room on this node, for operators.
func (m *Manager) AllRooms() []*Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		out = append(out, r)
	}
	return out
}

// Delete closes a room and removes it.
func (m *Manager) Delete(projectID, name string) error {
	key := roomKey{projectID, name}
	m.mu.Lock()
	r := m.rooms[key]
	delete(m.rooms, key)
	m.mu.Unlock()
	if r == nil {
		return ErrRoomNotFound
	}
	metrics.Rooms.Dec()
	r.close(pb.DisconnectReason_ROOM_DELETED)
	return nil
}

// Evict closes this node's copy of a room another node holds. Its people are told to connect again, not that it is over.
func (m *Manager) Evict(projectID, name string) {
	key := roomKey{projectID, name}
	m.mu.Lock()
	r := m.rooms[key]
	delete(m.rooms, key)
	m.mu.Unlock()
	if r == nil {
		return
	}
	metrics.Rooms.Dec()
	r.closeWith(pb.DisconnectReason_MIGRATION, (*participant.Participant).CloseToReconnect)
}

func (m *Manager) maintain(ctx context.Context) {
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case now := <-t.C:
			m.mu.Lock()
			var expired []*Room
			for key, r := range m.rooms {
				if r.idleExpired(now) {
					expired = append(expired, r)
					delete(m.rooms, key)
				}
			}
			m.mu.Unlock()
			for _, r := range expired {
				metrics.Rooms.Dec()
				go r.close(pb.DisconnectReason_ROOM_CLOSED)
			}
		}
	}
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	rooms := m.rooms
	m.rooms = map[roomKey]*Room{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rooms {
		wg.Add(1)
		go func(r *Room) {
			defer wg.Done()
			metrics.Rooms.Dec()
			r.close(pb.DisconnectReason_SERVER_SHUTDOWN)
		}(r)
	}
	wg.Wait()
}

// JoinRequest describes a client connecting to a room.
type JoinRequest struct {
	RoomName             string
	Verified             *auth.Verified
	Client               *pb.ClientInfo
	AutoSubscribe        bool
	AutoSubscribeData    bool
	AdaptiveStream       bool
	SubscriberAllowPause *bool
	// SinglePC is a client on the single peer connection protocol.
	SinglePC bool
	Sink     participant.Sink
}

// Join admits a participant to a room.
func (m *Manager) Join(req JoinRequest) (*participant.Participant, error) {
	project := req.Verified.Key.ProjectID
	if m.Draining() {
		return nil, ErrDraining
	}
	// Before the room: a refused token must not reopen a room that has closed.
	opts := m.optionsFromGrants(req.Verified.Grants)
	if m.admission != nil {
		admitted, err := m.admission.Admit(project, req.RoomName, req.Verified.Grants, true)
		if err != nil {
			return nil, err
		}
		// A meeting's own details count over what a token says. A token's own participant limit counts over a meeting's
		// default cap.
		if admitted.Metadata != "" {
			opts.Metadata = admitted.Metadata
		}
		opts.MaxParticipants = pick(opts.MaxParticipants, admitted.MaxParticipants)
	}
	autoCreate := m.cfg.Room.AutoCreate
	settings := m.ProjectConfigFor(project)
	if settings.AutoCreate != nil {
		autoCreate = *settings.AutoCreate
	}
	var room *Room
	if autoCreate {
		room, _ = m.GetOrCreate(project, req.RoomName, opts)
	} else {
		room = m.Get(project, req.RoomName)
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}

	g := req.Verified.Grants
	name := g.Name
	p, err := participant.New(participant.Params{
		SID:                        participant.NewID(participant.ParticipantPrefix),
		Identity:                   g.Identity,
		Name:                       name,
		Grants:                     g.Clone(),
		Key:                        req.Verified.Key,
		Client:                     req.Client,
		AutoSubscribe:              req.AutoSubscribe,
		AutoSubscribeData:          req.AutoSubscribeData,
		AdaptiveStream:             req.AdaptiveStream,
		SubscriberAllowPause:       req.SubscriberAllowPause,
		SinglePC:                   req.SinglePC,
		SyncStreams:                room.syncStreams,
		ScreenShareMaxPlayoutDelay: time.Duration(m.cfg.RTC.ScreenShareMaxPlayoutDelayMs) * time.Millisecond,
		Region:                     m.cfg.Region,
		ICEServers:                 m.iceServersFor(project, g.Identity, g.Video != nil && g.Video.Recorder),
		Engine:                     m.engine,
		Listener:                   room,
		Log:                        room.log,
		Development:                m.cfg.Development,
		MaxDataPacketBytes:         m.cfg.Limits.MaxDataPacketBytes,
		MaxDataMessagesPerSecond:   m.cfg.Limits.MaxDataMessagesPerSecond,
		MaxDataBytesPerSecond:      m.cfg.Limits.MaxDataBytesPerSecond,
		MaxMetadataBytes:           m.cfg.Limits.MaxMetadataBytes,
	}, req.Sink)
	if err != nil {
		return nil, err
	}
	if err := room.join(p); err != nil {
		p.Close(false, pb.DisconnectReason_JOIN_FAILURE)
		return nil, err
	}
	// The door said yes a moment ago, and they may have been removed since, when there was no session for that to close. A
	// removal records itself and then looks for a session, and this joins and then looks for a removal. So whichever comes
	// second finds the other, and nobody slips between them.
	if m.admission != nil {
		if _, err := m.admission.Admit(project, req.RoomName, req.Verified.Grants, false); err != nil {
			p.Close(false, pb.DisconnectReason_PARTICIPANT_REMOVED)
			return nil, err
		}
	}
	return p, nil
}

// ErrDraining means this node is not taking new sessions.
var ErrDraining = errors.New("server is draining")

// ErrCannotResume means there is no session to return to. The client must join again.
var ErrCannotResume = errors.New("no session to resume")

// Resume reconnects a client's signalling to its session.
func (m *Manager) Resume(req JoinRequest, participantSID string, reason pb.ReconnectReason) (*participant.Participant, error) {
	room := m.Get(req.Verified.Key.ProjectID, req.RoomName)
	if room == nil {
		return nil, ErrCannotResume
	}
	p := room.Participant(req.Verified.Grants.Identity)
	if p == nil || p.IsClosed() || (participantSID != "" && !strings.EqualFold(p.SID(), participantSID)) {
		return nil, ErrCannotResume
	}
	// One connection and two are not the same session, whatever the client now wants. It has to start again.
	if p.SinglePC() != req.SinglePC {
		return nil, ErrCannotResume
	}
	m.log.Info("participant resuming", "participant", p.Identity(), "reason", reason.String())
	p.Resume(req.Sink, &pb.ReconnectResponse{
		IceServers: m.iceServersFor(req.Verified.Key.ProjectID, req.Verified.Grants.Identity, p.Recorder()),
		ServerInfo: m.serverInfo(),
	})

	// Catch the client up on everything it may have missed.
	var updates []*pb.ParticipantInfo
	for _, other := range room.Participants() {
		if !other.Hidden() {
			updates = append(updates, other.ToProto())
		}
	}
	p.SendParticipantUpdate(updates)
	p.SendRoomUpdate(room.ToProto())
	p.ICERestart()
	return p, nil
}
