// Package participant implements one client's session: its signalling, its two peer connections, the
// tracks it publishes and the tracks it subscribes to.
package participant

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/datatrack"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/sfu"
	"elnova-streamkit.local/server/track"
	"elnova-streamkit.local/server/transport"
)

const (
	// The labels clients look for on the data channels the server opens.
	reliableChannel = "_reliable"
	lossyChannel    = "_lossy"

	// How long a participant survives without signalling, waiting for the client to resume.
	reconnectWindow = 20 * time.Second
	// Signal messages kept for a client that is reconnecting. Beyond this, resuming would leave the
	// client with a wrong picture of the room, so the session is closed instead.
	maxOutbox = 2048

	maxPendingData   = 1024
	tokenRefreshTime = 5 * time.Minute
	// How long to keep encoding a layer nobody has asked for. A publisher with no subscribers yet gets the
	// initial delay, since the first viewer usually arrives within moments.
	dynacastDownDelay    = 5 * time.Second
	dynacastInitialDelay = 10 * time.Second
	refreshTokenTTL      = 6 * time.Hour

	senderReportInterval = 3 * time.Second
	// One compound packet holds this many items: a report and two description items count as three per track.
	senderReportBatch      = 30
	senderReportBatchItems = 3
)

// ErrClosed is returned for operations on a closed participant.
var ErrClosed = errors.New("participant closed")

// Sink delivers signal messages to the client. Send must not block.
type Sink interface {
	Send(*pb.SignalResponse) error
	Close(reason string)
}

// Listener receives the events a participant raises for its room. Implementations may call back into
// the participant.
type Listener interface {
	OnParticipantChanged(p *Participant)
	OnParticipantActive(p *Participant)
	OnTrackPublished(p *Participant, t *track.Published)
	OnTrackReady(p *Participant, t *track.Published)
	OnTrackUnpublished(p *Participant, t *track.Published)
	OnDataPacket(p *Participant, kind pb.DataPacket_Kind, dp *pb.DataPacket)
	OnSubscriptionRequest(p *Participant, trackSIDs []string, subscribe bool)
	OnPermissionsChanged(p *Participant)
	OnClosed(p *Participant, reason pb.DisconnectReason)
	FindTrack(sid string) (*track.Published, *Participant)
	OnDataTrackPublished(p *Participant, t *datatrack.Track)
	OnDataTrackUnpublished(p *Participant, t *datatrack.Track)
	FindDataTrack(sid string) (*datatrack.Track, *Participant)
}

// Params configures a Participant.
type Params struct {
	SID           string
	Identity      string
	Name          string
	Grants        *auth.Grants
	Key           auth.Key
	Client        *pb.ClientInfo
	AutoSubscribe bool
	// AutoSubscribeData is the same for data tracks, and separate, because clients ask for those one at a time.
	AutoSubscribeData    bool
	AdaptiveStream       bool
	SubscriberAllowPause *bool
	// SinglePC is the single peer connection protocol: one connection carries media both ways, and the client makes every
	// offer. p.pub and p.sub are then the same Peer, whose Negotiate asks the client to offer.
	SinglePC bool
	// SyncStreams gives a publisher's audio and video one MediaStream ID on this subscriber's connection, which is what
	// lets a browser line them up. See sync_streams in the room configuration.
	SyncStreams bool
	// ScreenShareMaxPlayoutDelay limits how long this participant's browser may hold frames of a shared screen.
	ScreenShareMaxPlayoutDelay time.Duration
	Region                     string
	ICEServers                 []*pb.ICEServer
	Engine                     *transport.Engine
	Listener                   Listener
	Log                        *slog.Logger
	Development                bool
	MaxDataPacketBytes         int
	// MaxDataMessagesPerSecond and MaxDataBytesPerSecond limit what this participant can send to the room. Zero sets no limit.
	MaxDataMessagesPerSecond int
	MaxDataBytesPerSecond    int
	MaxMetadataBytes         int
}

type earlyStream struct {
	remote   *webrtc.TrackRemote
	receiver *webrtc.RTPReceiver
}

type subscription struct {
	track  *track.Published
	down   *sfu.DownTrack
	sender *webrtc.RTPSender
}

// Participant is one client's session.
type Participant struct {
	params Params
	log    *slog.Logger
	sid    string

	pub   *transport.Peer
	sub   *transport.Peer
	alloc *sfu.Allocator

	mu         sync.RWMutex
	state      pb.ParticipantInfo_State
	metadata   string
	name       string
	attributes map[string]string
	permission *pb.ParticipantPermission
	published  map[string]*track.Published
	pendingCID map[string]*track.Published
	// offeredByCID holds tracks seen in an offer before their AddTrack request, earlyMedia the streams
	// that arrived before it.
	offeredByCID map[string]offeredMedia
	earlyMedia   map[string][]earlyStream
	boundMids    map[string]bool
	subscribed   map[string]*subscription
	wanted       map[string]bool
	unwanted     map[string]bool
	joinedAt     time.Time
	closed       bool
	closeReason  pb.DisconnectReason
	dynacast     map[string]*dynacastState

	// Data tracks this participant publishes, by the handle its packets carry, and those it receives, by track ID.
	dataTracks      map[uint16]*pb.DataTrackInfo
	dataTrackRoutes map[uint16]*datatrack.Track
	dataSubs        map[string]*dataSubscription
	nextDataHandle  uint16

	// Who may subscribe to this participant's tracks.
	allowAll     bool
	allowedBySub map[string]map[string]bool // subscriber identity or SID -> track SIDs; "*" means all

	qualityScore float32
	quality      pb.ConnectionQuality

	version    atomic.Uint32
	everActive atomic.Bool

	sinkMu sync.Mutex
	sink   Sink
	outbox []*pb.SignalResponse

	reliable  *dataSender
	lossy     *dataSender
	dataTrack *dataSender

	lastReliableSeq atomic.Uint32
	// Nil when there is no limit.
	dataMessages    *rate.Limiter
	dataBytes       *rate.Limiter
	disconnectTimer *time.Timer
	refreshTimer    *time.Ticker
	stop            chan struct{}
}

// New creates a participant and its peer connections. Call SetSink before anything is sent.
func New(params Params, sink Sink) (*Participant, error) {
	p := &Participant{
		params:       params,
		sid:          params.SID,
		log:          params.Log.With("participant", params.Identity, "pID", params.SID),
		name:         params.Name,
		sink:         sink,
		state:        pb.ParticipantInfo_JOINING,
		metadata:     params.Grants.Metadata,
		attributes:   cloneMap(params.Grants.Attributes),
		permission:   permissionFromGrants(params.Grants.Video),
		published:    map[string]*track.Published{},
		pendingCID:   map[string]*track.Published{},
		offeredByCID: map[string]offeredMedia{},
		earlyMedia:   map[string][]earlyStream{},
		boundMids:    map[string]bool{},
		subscribed:   map[string]*subscription{},
		wanted:       map[string]bool{},
		unwanted:     map[string]bool{},
		dynacast:     map[string]*dynacastState{},
		allowAll:     true,
		allowedBySub: map[string]map[string]bool{},
		joinedAt:     time.Now(),
		stop:         make(chan struct{}),

		dataTracks:      map[uint16]*pb.DataTrackInfo{},
		dataTrackRoutes: map[uint16]*datatrack.Track{},
		dataSubs:        map[string]*dataSubscription{},
	}
	p.version.Store(1)
	p.setupDataLimits()
	if err := p.setupPeers(); err != nil {
		return nil, err
	}
	go p.refreshTokenLoop()
	go p.senderReportLoop()
	return p, nil
}

// setupDataLimits makes the limits on what this participant can send. Bursts are generous because real use is bursty: a
// client sends what built up in a reconnect all at once. A burst must also fit the largest message allowed, or that
// message could never go.
func (p *Participant) setupDataLimits() {
	if n := p.params.MaxDataMessagesPerSecond; n > 0 {
		p.dataMessages = rate.NewLimiter(rate.Limit(n), 2*n)
	}
	if n := p.params.MaxDataBytesPerSecond; n > 0 {
		p.dataBytes = rate.NewLimiter(rate.Limit(n), max(4*n, p.params.MaxDataPacketBytes))
	}
}

func (p *Participant) allowData(size int) bool {
	if p.dataMessages != nil && !p.dataMessages.Allow() {
		return false
	}
	return p.dataBytes == nil || p.dataBytes.AllowN(time.Now(), size)
}

func permissionFromGrants(v *auth.VideoGrant) *pb.ParticipantPermission {
	perm := &pb.ParticipantPermission{
		CanSubscribe:      v.GetCanSubscribe(),
		CanPublish:        v.GetCanPublish(),
		CanPublishData:    v.GetCanPublishData(),
		CanUpdateMetadata: v.GetCanUpdateOwnMetadata(),
		Hidden:            v.Hidden,
		Recorder:          v.Recorder,
		Agent:             v.Agent,
	}
	for _, s := range v.CanPublishSources {
		if src, ok := sourceFromName(s); ok {
			perm.CanPublishSources = append(perm.CanPublishSources, src)
		}
	}
	return perm
}

func sourceFromName(name string) (pb.TrackSource, bool) {
	v, ok := pb.TrackSource_value[strings.ToUpper(name)]
	return pb.TrackSource(v), ok
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (p *Participant) setupPeers() error {
	cfg := webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsUnifiedPlan}
	ice := func(candidate string, target pb.SignalTarget) {
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Trickle{Trickle: &pb.TrickleRequest{
			CandidateInit: candidate, Target: target,
		}}})
	}
	if p.params.SinglePC {
		if err := p.setupSinglePeer(cfg, ice); err != nil {
			return err
		}
		p.setupAllocator()
		return nil
	}
	pubPC, err := p.params.Engine.NewPublisherPC(cfg)
	if err != nil {
		return err
	}
	subPC, err := p.params.Engine.NewSubscriberPC(cfg)
	if err != nil {
		_ = pubPC.Close()
		return err
	}
	p.pub = transport.NewPeer(pubPC, pb.SignalTarget_PUBLISHER, transport.Callbacks{
		ICE:         ice,
		StateChange: func(s webrtc.PeerConnectionState) { p.onPeerState(pb.SignalTarget_PUBLISHER, s) },
	}, p.log)
	p.sub = transport.NewPeer(subPC, pb.SignalTarget_SUBSCRIBER, transport.Callbacks{
		ICE: ice,
		Offer: func(sd *pb.SessionDescription) {
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Offer{Offer: sd}})
		},
		StateChange: func(s webrtc.PeerConnectionState) { p.onPeerState(pb.SignalTarget_SUBSCRIBER, s) },
	}, p.log)

	pubPC.OnTrack(p.onMediaTrack)
	pubPC.OnDataChannel(p.onPublisherDataChannel)

	ordered := true
	noRetransmit := uint16(0)
	unordered := false
	reliable, err := subPC.CreateDataChannel(reliableChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	lossy, err := subPC.CreateDataChannel(lossyChannel, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &noRetransmit})
	if err != nil {
		return err
	}
	dataTrack, err := subPC.CreateDataChannel(datatrack.ChannelLabel, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &noRetransmit})
	if err != nil {
		return err
	}
	p.reliable = newDataSender(reliable)
	p.lossy = newDataSender(lossy)
	p.dataTrack = newDataSender(dataTrack)
	p.setupAllocator()
	return nil
}

func (p *Participant) setupAllocator() {
	p.alloc = sfu.NewAllocator(nil, p.onPauseChange)
	p.alloc.SetMaxBitrate(int64(p.params.Engine.MaxBitrate()))
	if allow := p.params.SubscriberAllowPause; allow != nil {
		p.alloc.SetAllowPause(*allow)
	}
}

// setupSinglePeer makes the one connection of the single peer connection protocol. The client makes the data channels on
// it, and they carry data both ways, so the senders wait for them to appear.
func (p *Participant) setupSinglePeer(cfg webrtc.Configuration, ice func(string, pb.SignalTarget)) error {
	pc, err := p.params.Engine.NewSinglePC(cfg)
	if err != nil {
		return err
	}
	p.pub = transport.NewPeer(pc, pb.SignalTarget_PUBLISHER, transport.Callbacks{
		ICE:         ice,
		StateChange: func(s webrtc.PeerConnectionState) { p.onPeerState(pb.SignalTarget_PUBLISHER, s) },
		Requirement: func(audio, video uint32) {
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_MediaSectionsRequirement{
				MediaSectionsRequirement: &pb.MediaSectionsRequirement{NumAudios: audio, NumVideos: video},
			}})
		},
	}, p.log)
	p.sub = p.pub
	p.reliable, p.lossy, p.dataTrack = newDataSender(nil), newDataSender(nil), newDataSender(nil)
	pc.OnTrack(p.onMediaTrack)
	pc.OnDataChannel(p.onPublisherDataChannel)
	return nil
}

// SinglePC reports whether this is a session on the single peer connection protocol.
func (p *Participant) SinglePC() bool { return p.params.SinglePC }

// SID returns the participant's server-assigned ID.
func (p *Participant) SID() string { return p.sid }

// Identity returns the participant's identity from its token.
func (p *Participant) Identity() string { return p.params.Identity }

// Hidden reports whether the participant is invisible to others.
func (p *Participant) Hidden() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.permission.GetHidden()
}

// Recorder reports whether the participant is recording the room.
func (p *Participant) Recorder() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.permission.GetRecorder()
}

// AutoSubscribe reports whether this participant receives new tracks without asking.
func (p *Participant) AutoSubscribe() bool { return p.params.AutoSubscribe }

// CanSubscribe reports the token's subscribe permission.
func (p *Participant) CanSubscribe() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.permission.GetCanSubscribe()
}

// SubscriberPrimary reports which connection the client treats as primary. A participant who can receive media
// waits on the subscriber connection, even with auto-subscribe off: the data channels it receives on are there.
// One that only publishes waits on the publisher connection.
func (p *Participant) SubscriberPrimary() bool { return !p.params.SinglePC && p.CanSubscribe() }

// IsClosed reports whether the session has ended.
func (p *Participant) IsClosed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.closed
}

// State returns the lifecycle state.
func (p *Participant) State() pb.ParticipantInfo_State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// ToProto returns the participant as others see it.
func (p *Participant) ToProto() *pb.ParticipantInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info := &pb.ParticipantInfo{
		Sid:              p.sid,
		Identity:         p.params.Identity,
		State:            p.state,
		Metadata:         p.metadata,
		JoinedAt:         p.joinedAt.Unix(),
		JoinedAtMs:       p.joinedAt.UnixMilli(),
		Name:             p.name,
		Version:          p.version.Load(),
		Permission:       proto.Clone(p.permission).(*pb.ParticipantPermission),
		Region:           p.params.Region,
		Attributes:       cloneMap(p.attributes),
		ClientProtocol:   p.params.Client.GetClientProtocol(),
		Kind:             kindFromGrants(p.params.Grants),
		DisconnectReason: p.closeReason,
	}
	for _, t := range p.published {
		info.Tracks = append(info.Tracks, t.Info())
		info.IsPublisher = true
	}
	// Clients treat this as the complete list every time, so a track missing from it counts as unpublished.
	for _, t := range p.dataTracks {
		info.DataTracks = append(info.DataTracks, proto.Clone(t).(*pb.DataTrackInfo))
		info.IsPublisher = true
	}
	return info
}

func kindFromGrants(g *auth.Grants) pb.ParticipantInfo_Kind {
	if v, ok := pb.ParticipantInfo_Kind_value[strings.ToUpper(g.Kind)]; ok {
		return pb.ParticipantInfo_Kind(v)
	}
	if g.Video.Agent {
		return pb.ParticipantInfo_AGENT
	}
	return pb.ParticipantInfo_STANDARD
}

func (p *Participant) changed() {
	p.version.Add(1)
	p.params.Listener.OnParticipantChanged(p)
}

// SetState moves the lifecycle forward. Sending join moves JOINING to JOINED, and the primary
// connection coming up moves JOINED to ACTIVE.
func (p *Participant) SetState(s pb.ParticipantInfo_State) {
	p.mu.Lock()
	if s <= p.state && s != pb.ParticipantInfo_DISCONNECTED || p.state == pb.ParticipantInfo_DISCONNECTED {
		p.mu.Unlock()
		return
	}
	p.state = s
	if s == pb.ParticipantInfo_ACTIVE {
		p.everActive.Store(true)
	}
	p.mu.Unlock()
	p.log.Debug("participant state", "state", s.String())
	p.changed()
	if s == pb.ParticipantInfo_ACTIVE {
		p.params.Listener.OnParticipantActive(p)
	}
}

// WasActive reports whether the primary connection ever came up.
func (p *Participant) WasActive() bool { return p.everActive.Load() }

func (p *Participant) onPeerState(target pb.SignalTarget, s webrtc.PeerConnectionState) {
	switch s {
	case webrtc.PeerConnectionStateConnected:
		p.cancelDisconnectTimer()
		primary := pb.SignalTarget_PUBLISHER
		if p.SubscriberPrimary() {
			primary = pb.SignalTarget_SUBSCRIBER
		}
		if target == primary {
			p.SetState(pb.ParticipantInfo_ACTIVE)
		}
	case webrtc.PeerConnectionStateFailed:
		// The client restarts ICE by resuming, so give it the same window as a lost signal connection.
		p.log.Info("peer connection failed", "target", target.String())
		p.startDisconnectTimer(pb.DisconnectReason_CONNECTION_TIMEOUT)
	}
}

// Send queues a message for the client, or holds it until the client resumes.
func (p *Participant) Send(msg *pb.SignalResponse) {
	p.sinkMu.Lock()
	defer p.sinkMu.Unlock()
	if p.sink != nil {
		if err := p.sink.Send(msg); err == nil {
			return
		}
		p.log.Debug("signal send failed, holding messages for resume")
		p.sink = nil
	}
	if len(p.outbox) >= maxOutbox {
		p.log.Warn("signal backlog is full; a resume would see stale state")
		return
	}
	p.outbox = append(p.outbox, msg)
}

// SendJoin queues the join response. It must be the first message on a new connection, and it only
// queues, so the room can call it with its lock held.
func (p *Participant) SendJoin(join *pb.JoinResponse) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Join{Join: join}})
}

// Joined starts the session after the join response is queued.
func (p *Participant) Joined() {
	p.SetState(pb.ParticipantInfo_JOINED)
	p.sendRefreshToken()
	if p.SubscriberPrimary() {
		// The client connects the subscriber connection first and waits on it, so offer right away.
		p.sub.Negotiate(false)
	}
}

func (p *Participant) sendRefreshToken() {
	grants := p.params.Grants.Clone()
	p.mu.RLock()
	grants.Name = p.name
	grants.Metadata = p.metadata
	grants.Attributes = cloneMap(p.attributes)
	p.mu.RUnlock()
	token, err := auth.Mint(p.params.Key, grants, refreshTokenTTL)
	if err != nil {
		p.log.Warn("could not mint refresh token", "error", err)
		return
	}
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_RefreshToken{RefreshToken: token}})
}

func (p *Participant) refreshTokenLoop() {
	t := time.NewTicker(tokenRefreshTime)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.sendRefreshToken()
		}
	}
}

// Resume attaches a new signalling connection to this session. The caller then sends room state.
func (p *Participant) Resume(sink Sink, response *pb.ReconnectResponse) {
	p.cancelDisconnectTimer()
	response.LastMessageSeq = p.lastReliableSeq.Load()

	p.sinkMu.Lock()
	old := p.sink
	p.sink = sink
	pending := p.outbox
	p.outbox = nil
	// The reconnect response must come first; clients treat anything else as a failed resume.
	_ = sink.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Reconnect{Reconnect: response}})
	for _, m := range pending {
		_ = sink.Send(m)
	}
	p.sinkMu.Unlock()
	if old != nil {
		old.Close("replaced by resume")
	}
}

// ICERestart makes a new offer with fresh ICE credentials on the subscriber connection. The client
// restarts ICE on the publisher connection itself.
func (p *Participant) ICERestart() {
	// With one connection the client restarts ICE by offering, and the answer needs nothing special.
	if p.params.SinglePC {
		return
	}
	// An offer still unanswered from before the connection dropped has to be answered first.
	p.sub.ResendPendingOffer()
	p.sub.Negotiate(true)
}

// SignalClosed records that the signalling connection went away. The media connections may well still
// be up, so the session lives on for a while, waiting for the client to come back.
func (p *Participant) SignalClosed(sink Sink) {
	p.sinkMu.Lock()
	if p.sink != sink {
		p.sinkMu.Unlock()
		return
	}
	p.sink = nil
	p.sinkMu.Unlock()
	p.startDisconnectTimer(pb.DisconnectReason_SIGNAL_CLOSE)
}

func (p *Participant) startDisconnectTimer(reason pb.DisconnectReason) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.disconnectTimer != nil {
		return
	}
	p.disconnectTimer = time.AfterFunc(reconnectWindow, func() {
		p.log.Info("no reconnect in time, closing session", "reason", reason.String())
		p.Close(false, reason)
	})
}

func (p *Participant) cancelDisconnectTimer() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disconnectTimer != nil {
		p.disconnectTimer.Stop()
		p.disconnectTimer = nil
	}
}

// Close ends the session. With sendLeave, the client is told why first.
func (p *Participant) Close(sendLeave bool, reason pb.DisconnectReason) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.closeReason = reason
	if p.disconnectTimer != nil {
		p.disconnectTimer.Stop()
		p.disconnectTimer = nil
	}
	published := make([]*track.Published, 0, len(p.published))
	for _, t := range p.published {
		published = append(published, t)
	}
	subs := make([]*subscription, 0, len(p.subscribed))
	for _, s := range p.subscribed {
		subs = append(subs, s)
	}
	p.subscribed = map[string]*subscription{}
	p.mu.Unlock()

	metrics.SessionsEnded.WithLabelValues(reason.String()).Inc()
	close(p.stop)
	p.log.Info("participant closing", "reason", reason.String())

	if sendLeave {
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Leave{Leave: &pb.LeaveRequest{
			Reason: reason, Action: pb.LeaveRequest_DISCONNECT,
		}}})
	}

	p.mu.Lock()
	for _, state := range p.dynacast {
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	p.mu.Unlock()
	p.alloc.Close()
	for _, s := range subs {
		s.track.RemoveSubscriber(p.sid)
		s.down.Close()
		metrics.TracksOut.Dec()
	}
	p.leaveDataSubscriptions()
	p.unpublishAllDataTracks(false)
	p.mu.Lock()
	p.state = pb.ParticipantInfo_DISCONNECTED
	p.mu.Unlock()
	p.version.Add(1)
	for _, t := range published {
		if r := t.Receiver(); r != nil {
			r.Close()
		}
	}
	p.sub.Close()
	p.pub.Close()

	p.sinkMu.Lock()
	sink := p.sink
	p.sink = nil
	p.sinkMu.Unlock()
	if sink != nil {
		sink.Close(reason.String())
	}
	p.params.Listener.OnClosed(p, reason)
}

// CloseToReconnect ends this session and tells the client to start another, because what it is connected to is moving.
func (p *Participant) CloseToReconnect() {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Leave{Leave: &pb.LeaveRequest{
		Reason: pb.DisconnectReason_MIGRATION, Action: pb.LeaveRequest_RECONNECT, CanReconnect: true,
	}}})
	p.Close(false, pb.DisconnectReason_MIGRATION)
}

// HandleRequest applies one message from the client. Requests from one client run in order.
func (p *Participant) HandleRequest(req *pb.SignalRequest) {
	switch m := req.GetMessage().(type) {
	case *pb.SignalRequest_Offer:
		p.handleOffer(m.Offer)
	case *pb.SignalRequest_Answer:
		if p.params.SinglePC {
			p.log.Debug("ignoring an answer: this server makes no offers on one connection")
			return
		}
		if err := p.sub.HandleAnswer(m.Answer); err != nil {
			p.log.Warn("could not apply answer", "error", err)
		}
	case *pb.SignalRequest_Trickle:
		switch m.Trickle.GetTarget() {
		case pb.SignalTarget_PUBLISHER:
			p.pub.AddICE(m.Trickle.GetCandidateInit())
		case pb.SignalTarget_SUBSCRIBER:
			p.sub.AddICE(m.Trickle.GetCandidateInit())
		}
	case *pb.SignalRequest_AddTrack:
		p.handleAddTrack(m.AddTrack)
	case *pb.SignalRequest_Mute:
		p.handleMute(m.Mute)
	case *pb.SignalRequest_Subscription:
		p.handleSubscription(m.Subscription)
	case *pb.SignalRequest_TrackSetting:
		p.handleTrackSettings(m.TrackSetting)
	case *pb.SignalRequest_Leave:
		p.Close(false, pb.DisconnectReason_CLIENT_INITIATED)
	case *pb.SignalRequest_SubscriptionPermission:
		p.handleSubscriptionPermission(m.SubscriptionPermission)
	case *pb.SignalRequest_SyncState:
		p.handleSyncState(m.SyncState)
	case *pb.SignalRequest_UpdateMetadata:
		p.handleUpdateMetadata(m.UpdateMetadata)
	case *pb.SignalRequest_UpdateVideoTrack:
		p.updateTrackInfo(m.UpdateVideoTrack.GetTrackSid(), func(i *pb.TrackInfo) {
			i.Width, i.Height = m.UpdateVideoTrack.GetWidth(), m.UpdateVideoTrack.GetHeight()
		})
	case *pb.SignalRequest_UpdateAudioTrack:
		p.updateTrackInfo(m.UpdateAudioTrack.GetTrackSid(), func(i *pb.TrackInfo) {
			i.AudioFeatures = m.UpdateAudioTrack.GetFeatures()
		})
	case *pb.SignalRequest_Simulate:
		p.handleSimulate(m.Simulate)
	case *pb.SignalRequest_PublishDataTrackRequest:
		p.handlePublishDataTrack(m.PublishDataTrackRequest)
	case *pb.SignalRequest_UnpublishDataTrackRequest:
		p.handleUnpublishDataTrack(m.UnpublishDataTrackRequest)
	case *pb.SignalRequest_UpdateDataSubscription:
		p.handleDataSubscription(m.UpdateDataSubscription)
	case *pb.SignalRequest_StoreDataBlobRequest:
		// Data blobs are not built. These are the reference's answers with that feature off, which is its default.
		p.sendRequestResponse(&pb.RequestResponse{
			RequestId: m.StoreDataBlobRequest.GetRequestId(),
			Reason:    pb.RequestResponse_NOT_ALLOWED,
			Message:   "data blob not enabled",
		})
	case *pb.SignalRequest_GetDataBlobRequest:
		p.sendRequestResponse(&pb.RequestResponse{
			RequestId: m.GetDataBlobRequest.GetRequestId(),
			Reason:    pb.RequestResponse_NOT_FOUND,
			Message:   "data blob not found",
		})
	case *pb.SignalRequest_Ping, *pb.SignalRequest_PingReq:
		// Answered on the connection itself.
	default:
		p.log.Debug("unsupported signal request", "type", msgType(req))
	}
}

func msgType(req *pb.SignalRequest) string {
	return string(req.ProtoReflect().WhichOneof(req.ProtoReflect().Descriptor().Oneofs().ByName("message")).Name())
}

func (p *Participant) updateTrackInfo(sid string, fn func(*pb.TrackInfo)) {
	p.mu.RLock()
	t := p.published[sid]
	p.mu.RUnlock()
	if t == nil {
		return
	}
	t.Update(fn)
	p.changed()
}

func (p *Participant) handleOffer(sd *pb.SessionDescription) {
	answer, err := p.pub.Answer(sd, func() { p.preferRequestedCodecs(sd.GetSdp()) })
	if err != nil {
		p.log.Warn("could not answer offer", "error", err)
		return
	}
	media := parseOfferedMedia(sd.GetSdp(), answer.GetSdp())
	p.bindOfferedTracks(media)
	p.endRemovedTracks(media)
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Answer{Answer: answer}})
	p.pub.AnswerSent()
	p.resendSubscribedQualities()
}

// preferRequestedCodecs puts the codec each video track asked for first in the coming answer. A browser's offer lists
// VP8 first whatever the application asked for, and it then sends what the answer lists first, so without this a
// client asking for VP9 publishes VP8.
func (p *Participant) preferRequestedCodecs(offer string) {
	var desc sdp.SessionDescription
	if desc.Unmarshal([]byte(offer)) != nil {
		return
	}
	transceivers := p.pub.PC().GetTransceivers()
	for _, m := range desc.MediaDescriptions {
		if m.MediaName.Media != "video" {
			continue
		}
		mid, _ := m.Attribute("mid")
		msid, _ := m.Attribute("msid")
		parts := strings.Fields(msid)
		if mid == "" || len(parts) < 2 {
			continue
		}
		mime := p.requestedCodec(parts[1])
		if mime == "" {
			continue
		}
		for _, tr := range transceivers {
			if tr.Mid() != mid {
				continue
			}
			found, err := transport.PreferCodec(tr, mime, !p.followsAnswerCodecOrder())
			if err != nil {
				p.log.Warn("could not prefer codec", "codec", mime, "error", err)
			} else if !found {
				p.log.Info("offer does not carry the requested codec", "codec", mime, "mid", mid)
			}
			break
		}
	}
}

// requestedCodec returns the codec a client asked for on the track it knows by cid, or "" if it did not say.
func (p *Participant) requestedCodec(cid string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t := p.pendingCID[cid]
	if t == nil {
		for _, published := range p.published {
			if published.CID() == cid {
				t = published
				break
			}
		}
	}
	if t == nil {
		return ""
	}
	return t.RequestedCodec()
}

// followsAnswerCodecOrder reports whether this client sends the first codec in an answer. Firefox on Linux and Android
// does not, so it gets the requested codec alone.
func (p *Participant) followsAnswerCodecOrder() bool {
	client := p.params.Client
	firefox := strings.HasPrefix(strings.ToLower(client.GetBrowser()), "firefox")
	os := strings.ToLower(client.GetOs())
	return !firefox || (os != "linux" && os != "android")
}

// offeredMedia describes one audio or video m-section of a publisher offer.
type offeredMedia struct {
	mid     string
	trackID string
	kind    string
	mime    string
	// sending is false when the client stopped sending on this m-section. Clients have no unpublish
	// message: this is how the server learns that a track was removed.
	sending bool
}

// parseOfferedMedia reads which client track each m-section carries from the offer, and which codec
// was chosen from the answer.
func parseOfferedMedia(offer, answer string) []offeredMedia {
	var o, a sdp.SessionDescription
	if o.Unmarshal([]byte(offer)) != nil || a.Unmarshal([]byte(answer)) != nil {
		return nil
	}
	var out []offeredMedia
	for i, m := range o.MediaDescriptions {
		kind := m.MediaName.Media
		if kind != "audio" && kind != "video" {
			continue
		}
		om := offeredMedia{kind: kind}
		om.mid, _ = m.Attribute("mid")
		if msid, ok := m.Attribute("msid"); ok {
			if parts := strings.Fields(msid); len(parts) >= 2 {
				om.trackID = parts[1]
			}
		}
		_, sendOnly := m.Attribute("sendonly")
		_, sendRecv := m.Attribute("sendrecv")
		// Port 0 means a rejected section, unless it is bundle-only: Firefox offers its bundled sections that way.
		_, bundleOnly := m.Attribute("bundle-only")
		om.sending = (sendOnly || sendRecv) && (m.MediaName.Port.Value != 0 || bundleOnly)
		if i < len(a.MediaDescriptions) {
			om.mime = mimeFromAnswer(a.MediaDescriptions[i], kind)
		}
		out = append(out, om)
	}
	return out
}

func mimeFromAnswer(m *sdp.MediaDescription, kind string) string {
	for _, f := range m.MediaName.Formats {
		for _, attr := range m.Attributes {
			if attr.Key != "rtpmap" || !strings.HasPrefix(attr.Value, f+" ") {
				continue
			}
			name, _, _ := strings.Cut(strings.TrimPrefix(attr.Value, f+" "), "/")
			return kind + "/" + name
		}
	}
	return ""
}

// bindOfferedTracks connects m-sections to AddTrack requests. Clients do not always wait for the
// AddTrack response before offering, so an offered track with no request yet is remembered for when the
// request arrives.
func (p *Participant) bindOfferedTracks(media []offeredMedia) {
	var ready []*track.Published
	p.mu.Lock()
	for _, m := range media {
		if !m.sending || m.trackID == "" || p.boundMids[m.mid] {
			continue
		}
		t := p.pendingCID[m.trackID]
		if t == nil && p.usesOwnMsid() {
			t = p.pendingForFallback(m.kind)
		}
		if t == nil {
			p.offeredByCID[m.trackID] = m
			continue
		}
		ready = append(ready, p.bindLocked(t, m))
	}
	p.mu.Unlock()
	p.announce(ready)
}

// usesOwnMsid reports whether this client's SDP track IDs differ from the IDs it sends in AddTrack.
// Firefox generates its own.
func (p *Participant) usesOwnMsid() bool {
	return strings.Contains(strings.ToLower(p.params.Client.GetBrowser()), "firefox")
}

func (p *Participant) bindLocked(t *track.Published, m offeredMedia) *track.Published {
	delete(p.pendingCID, t.CID())
	delete(p.offeredByCID, m.trackID)
	p.boundMids[m.mid] = true
	t.Update(func(i *pb.TrackInfo) {
		i.Mid = m.mid
		i.MimeType = m.mime
		for _, c := range i.Codecs {
			c.Mid = m.mid
			c.MimeType = m.mime
		}
	})
	p.published[t.SID()] = t
	return t
}

// announce tells the room about newly bound tracks, then starts any media that got here first.
func (p *Participant) announce(ready []*track.Published) {
	for _, t := range ready {
		metrics.TracksIn.Inc()
		p.version.Add(1)
		p.params.Listener.OnTrackPublished(p, t)

		p.mu.Lock()
		early := p.earlyMedia[t.CID()]
		delete(p.earlyMedia, t.CID())
		p.mu.Unlock()
		for _, e := range early {
			p.startMedia(t, e.remote, e.receiver)
		}
	}
}

// pendingForFallback finds the only pending track of a kind, for clients whose SDP track IDs cannot be
// matched. With several candidates there is no way to tell, so it finds nothing.
func (p *Participant) pendingForFallback(kind string) *track.Published {
	var found *track.Published
	for _, t := range p.pendingCID {
		if (kind == "audio") != (t.Kind() == pb.TrackType_AUDIO) {
			continue
		}
		if found != nil {
			return nil
		}
		found = t
	}
	return found
}

// endRemovedTracks ends tracks whose m-section the client has stopped sending on.
func (p *Participant) endRemovedTracks(media []offeredMedia) {
	var ended []*track.Published
	p.mu.Lock()
	for _, m := range media {
		if m.sending {
			continue
		}
		delete(p.boundMids, m.mid)
		for _, t := range p.published {
			if t.Info().GetMid() == m.mid {
				ended = append(ended, t)
			}
		}
	}
	p.mu.Unlock()
	for _, t := range ended {
		p.log.Debug("client stopped sending track", "track", t.SID())
		if r := t.Receiver(); r != nil {
			r.Close()
		} else {
			p.onTrackEnded(t)
		}
	}
}

func sourceName(s pb.TrackSource) string { return strings.ToLower(s.String()) }

func (p *Participant) handleAddTrack(req *pb.AddTrackRequest) {
	if !p.params.Grants.Video.CanPublishSource(sourceName(req.GetSource())) {
		p.log.Info("publish denied", "source", req.GetSource().String())
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_RequestResponse{RequestResponse: &pb.RequestResponse{
			Reason:  pb.RequestResponse_NOT_ALLOWED,
			Message: "does not have permission to publish",
			Request: &pb.RequestResponse_AddTrack{AddTrack: req},
		}}})
		return
	}

	p.mu.RLock()
	existing := p.pendingCID[req.GetCid()]
	p.mu.RUnlock()
	if existing != nil {
		// A retry after a lost response, so repeat the answer.
		p.sendTrackPublished(req.GetCid(), existing)
		return
	}

	info := &pb.TrackInfo{
		Sid:               NewID(TrackIDPrefix(req.GetType(), req.GetSource())),
		Type:              req.GetType(),
		Name:              req.GetName(),
		Width:             req.GetWidth(),
		Height:            req.GetHeight(),
		Muted:             req.GetMuted(),
		Source:            req.GetSource(),
		Stream:            streamFor(req),
		DisableRed:        req.GetDisableRed(),
		Encryption:        req.GetEncryption(),
		AudioFeatures:     req.GetAudioFeatures(),
		BackupCodecPolicy: req.GetBackupCodecPolicy(),
	}
	requested := requestedVideoCodec(req)
	mode := videoLayerMode(req, requested)
	if req.GetType() == pb.TrackType_VIDEO {
		for _, l := range req.GetLayers() {
			copied := proto.Clone(l).(*pb.VideoLayer)
			copied.SpatialLayer = int32(l.GetQuality())
			if mode != pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
				copied.Rid = ridForQuality(l.GetQuality())
			} else {
				// All spatial layers share one RTP stream, so none of them has a RID of its own.
				copied.Rid = ""
			}
			info.Layers = append(info.Layers, copied)
		}
		if mode == pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
			slices.SortFunc(info.Layers, func(a, b *pb.VideoLayer) int { return int(a.GetSpatialLayer()) - int(b.GetSpatialLayer()) })
		} else {
			info.Simulcast = len(info.Layers) > 1
		}
	}
	info.Codecs = []*pb.SimulcastCodecInfo{{
		Cid:            req.GetCid(),
		MimeType:       requested,
		Layers:         info.Layers,
		VideoLayerMode: mode,
	}}

	t := track.New(p.sid, req.GetCid(), info)
	p.mu.Lock()
	p.pendingCID[req.GetCid()] = t
	var ready []*track.Published
	if m, offered := p.offeredByCID[req.GetCid()]; offered {
		ready = append(ready, p.bindLocked(t, m))
	}
	p.mu.Unlock()
	p.sendTrackPublished(req.GetCid(), t)
	p.announce(ready)
}

// requestedVideoCodec returns the mime type of the codec a video track asked for, or "" if it did not say. A client
// names its backup codec in the same list under another cid, so the entry for this cid wins, then the first.
func requestedVideoCodec(req *pb.AddTrackRequest) string {
	if req.GetType() != pb.TrackType_VIDEO {
		return ""
	}
	name := ""
	for _, c := range req.GetSimulcastCodecs() {
		if name == "" || c.GetCid() == req.GetCid() {
			name = c.GetCodec()
		}
		if c.GetCid() == req.GetCid() {
			break
		}
	}
	if name == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(name), "video/") {
		name = "video/" + name
	}
	return name
}

// videoLayerMode decides how a track's layers map to RTP streams. Official clients leave it unset for SVC. VP9 and AV1
// get one stream carrying every spatial layer unless the client says otherwise, because that is what those clients
// send whenever they ask for either codec.
func videoLayerMode(req *pb.AddTrackRequest, requested string) pb.VideoLayer_Mode {
	for _, c := range req.GetSimulcastCodecs() {
		if c.GetCid() == req.GetCid() && c.GetVideoLayerMode() != pb.VideoLayer_MODE_UNUSED {
			return c.GetVideoLayerMode()
		}
	}
	if requested == "" {
		// Nothing is known yet. Media settles it: see Published.ResolveLayerMode.
		return pb.VideoLayer_MODE_UNUSED
	}
	if sfu.IsSVCCodec(requested) {
		return pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM
	}
	return pb.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM
}

// streamFor groups tracks for clients: camera with microphone, screen with its audio.
func streamFor(req *pb.AddTrackRequest) string {
	if s := req.GetStream(); s != "" {
		return s
	}
	switch req.GetSource() {
	case pb.TrackSource_SCREEN_SHARE, pb.TrackSource_SCREEN_SHARE_AUDIO:
		return "screen"
	}
	return "camera"
}

func ridForQuality(q pb.VideoQuality) string {
	switch q {
	case pb.VideoQuality_LOW:
		return "q"
	case pb.VideoQuality_MEDIUM:
		return "h"
	}
	return "f"
}

func (p *Participant) sendTrackPublished(cid string, t *track.Published) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_TrackPublished{
		TrackPublished: &pb.TrackPublishedResponse{Cid: cid, Track: t.Info()},
	}})
}

// onMediaTrack runs once per incoming RTP stream: once for each simulcast layer of a track.
func (p *Participant) onMediaTrack(remote *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	p.mu.Lock()
	t := p.findTrackForRemoteLocked(remote, rtpReceiver)
	if t == nil {
		// Media can overtake the AddTrack request. Hold on to it until the request arrives.
		p.earlyMedia[remote.ID()] = append(p.earlyMedia[remote.ID()], earlyStream{remote, rtpReceiver})
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.startMedia(t, remote, rtpReceiver)
}

// startMedia attaches an incoming stream to its track.
func (p *Participant) startMedia(t *track.Published, remote *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	codec := remote.Codec()
	rid := remote.RID()
	layer := sfu.LayerForRID(rid)

	receiver := t.Receiver()
	isNew := receiver == nil
	if isNew {
		ext := sfu.HeaderExtensionIDs{}
		for _, h := range rtpReceiver.GetParameters().HeaderExtensions {
			switch h.URI {
			case sdp.AudioLevelURI:
				ext.AudioLevel = uint8(h.ID)
			case sfu.DependencyDescriptorURI:
				ext.DependencyDescriptor = uint8(h.ID)
			}
		}
		// A client may not have named a codec, in which case this is the first anyone knows of it.
		mode := t.ResolveLayerMode(sfu.IsSVCCodec(codec.MimeType) && rid == "")
		receiver = sfu.NewReceiver(sfu.ReceiverParams{
			Kind:      remote.Kind(),
			TrackID:   t.SID(),
			Codec:     codec,
			IsSVC:     remote.Kind() == webrtc.RTPCodecTypeVideo && rid == "" && mode == pb.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM,
			HeaderExt: ext,
			RequestKeyframe: func(ssrc uint32) {
				_ = p.pub.PC().WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
			},
			OnClosed: func() { p.onTrackEnded(t) },
		})
		receiver.SetMuted(t.Muted())
		t.SetReceiver(receiver)
		t.Update(func(i *pb.TrackInfo) {
			i.MimeType = codec.MimeType
			for _, c := range i.Codecs {
				c.MimeType = codec.MimeType
			}
		})
	}
	t.Update(func(i *pb.TrackInfo) {
		for _, l := range i.Layers {
			// With SVC no layer has a RID, and the one stream is reported on the first layer only.
			if l.GetRid() == rid && (rid != "" || l.GetSpatialLayer() == 0) {
				l.Ssrc = uint32(remote.SSRC())
			}
		}
	})

	receiver.AddLayer(layer, uint32(remote.SSRC()), func(b []byte) (int, error) {
		n, _, err := remote.Read(b)
		return n, err
	})
	// RTCP has to be read for the receiver reports to see the publisher's sender reports, and they are also what the
	// reports to subscribers are made from.
	go readPublisherRTCP(rtpReceiver, rid, receiver)

	if isNew {
		p.changed()
		p.params.Listener.OnTrackReady(p, t)
		if t.Kind() == pb.TrackType_VIDEO {
			time.AfterFunc(dynacastInitialDelay, func() { p.updateSubscribedQuality(t) })
		}
	} else {
		p.notifySubscribers(t)
	}
}

func readPublisherRTCP(r *webrtc.RTPReceiver, rid string, receiver *sfu.Receiver) {
	for {
		var pkts []rtcp.Packet
		var err error
		if rid != "" {
			pkts, _, err = r.ReadSimulcastRTCP(rid)
		} else {
			pkts, _, err = r.ReadRTCP()
		}
		if err != nil {
			return
		}
		now := time.Now()
		for _, pkt := range pkts {
			// A compound packet can carry reports about other streams, including retransmission. SetSenderReport keeps
			// only those for a stream this track receives.
			if sr, ok := pkt.(*rtcp.SenderReport); ok {
				receiver.SetSenderReport(sr, now)
			}
		}
	}
}

func (p *Participant) notifySubscribers(t *track.Published) { p.updateSubscribedQuality(t) }

func (p *Participant) findTrackForRemoteLocked(remote *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) *track.Published {
	var mid string
	for _, tr := range p.pub.PC().GetTransceivers() {
		if tr.Receiver() == rtpReceiver {
			mid = tr.Mid()
			break
		}
	}
	for _, t := range p.published {
		if t.CID() == remote.ID() || (mid != "" && t.Info().GetMid() == mid) {
			return t
		}
	}
	return nil
}

func (p *Participant) onTrackEnded(t *track.Published) {
	p.mu.Lock()
	if _, ok := p.published[t.SID()]; !ok {
		p.mu.Unlock()
		return
	}
	delete(p.published, t.SID())
	if state := p.dynacast[t.SID()]; state != nil {
		if state.timer != nil {
			state.timer.Stop()
		}
		delete(p.dynacast, t.SID())
	}
	closed := p.closed
	p.mu.Unlock()
	metrics.TracksIn.Dec()
	p.version.Add(1)
	if !closed {
		p.log.Debug("track ended", "track", t.SID())
	}
	p.params.Listener.OnTrackUnpublished(p, t)
}

// UnpublishTrack is the server-side removal of a track, for example by an administrator.
func (p *Participant) UnpublishTrack(sid string) error {
	p.mu.RLock()
	t := p.published[sid]
	p.mu.RUnlock()
	if t == nil {
		return ErrTrackNotFound
	}
	if r := t.Receiver(); r != nil {
		r.Close()
	} else {
		p.onTrackEnded(t)
	}
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_TrackUnpublished{
		TrackUnpublished: &pb.TrackUnpublishedResponse{TrackSid: sid},
	}})
	return nil
}

// ErrTrackNotFound is returned for an unknown track ID.
var ErrTrackNotFound = errors.New("track not found")

func (p *Participant) handleMute(m *pb.MuteTrackRequest) {
	p.setTrackMuted(m.GetSid(), m.GetMuted(), false)
}

// SetTrackMuted is an administrator's mute. The client is told to apply it.
func (p *Participant) SetTrackMuted(sid string, muted bool) (*pb.TrackInfo, error) {
	return p.setTrackMuted(sid, muted, true)
}

func (p *Participant) setTrackMuted(sid string, muted, fromServer bool) (*pb.TrackInfo, error) {
	p.mu.RLock()
	t := p.published[sid]
	if t == nil {
		// The client has been told this track's SID, even if it has not offered the track yet, so the SID must work.
		for _, pending := range p.pendingCID {
			if pending.SID() == sid {
				t = pending
				break
			}
		}
	}
	p.mu.RUnlock()
	if t == nil {
		return nil, ErrTrackNotFound
	}
	if t.Muted() != muted {
		t.Update(func(i *pb.TrackInfo) { i.Muted = muted })
		p.changed()
	}
	if r := t.Receiver(); r != nil {
		r.SetMuted(muted)
	}
	if fromServer {
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Mute{Mute: &pb.MuteTrackRequest{Sid: sid, Muted: muted}}})
	}
	return t.Info(), nil
}

func (p *Participant) handleSubscription(u *pb.UpdateSubscription) {
	sids := append([]string{}, u.GetTrackSids()...)
	for _, pt := range u.GetParticipantTracks() {
		sids = append(sids, pt.GetTrackSids()...)
	}
	p.params.Listener.OnSubscriptionRequest(p, sids, u.GetSubscribe())
}

// Wanted reports whether this participant should receive a track, given auto-subscribe and its explicit
// requests.
func (p *Participant) Wanted(trackSID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.unwanted[trackSID] {
		return false
	}
	return p.params.AutoSubscribe || p.wanted[trackSID]
}

// SetWanted records an explicit subscribe or unsubscribe request.
func (p *Participant) SetWanted(trackSID string, want bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if want {
		p.wanted[trackSID] = true
		delete(p.unwanted, trackSID)
	} else {
		p.unwanted[trackSID] = true
		delete(p.wanted, trackSID)
	}
}

// Subscribe starts sending a track to this participant. It is a no-op if already subscribed.
func (p *Participant) Subscribe(t *track.Published) error {
	receiver := t.Receiver()
	if receiver == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if _, ok := p.subscribed[t.SID()]; ok {
		p.mu.Unlock()
		return nil
	}
	if !p.permission.GetCanSubscribe() {
		p.mu.Unlock()
		return nil
	}
	s := &subscription{track: t}
	p.subscribed[t.SID()] = s
	p.mu.Unlock()

	owner := t.OwnerSID()
	var maxPlayoutDelay time.Duration
	if t.Source() == pb.TrackSource_SCREEN_SHARE {
		maxPlayoutDelay = p.params.ScreenShareMaxPlayoutDelay
	}
	streamID := owner + "|" + t.SID()
	shareStream := p.sharesStreamIDs() && t.Info().GetStream() != ""
	if shareStream {
		// Clients take the track ID from the second part of the msid, so this changes nothing for them. Browsers only line
		// up audio and video that are in one MediaStream.
		streamID = owner + "|" + t.Info().GetStream()
	}
	down := sfu.NewDownTrack(sfu.DownTrackParams{
		ID:              t.SID(),
		StreamID:        streamID,
		Receiver:        receiver,
		MaxPlayoutDelay: maxPlayoutDelay,
		OnBound:         func() { p.trackBound(t) },
		OnFirstPacket:   func() { p.sendStreamState(owner, t.SID(), pb.StreamState_ACTIVE) },
		OnClosed:        func() { p.onDownTrackClosed(t) },
		OnUnsupported: func() {
			p.log.Info("subscriber cannot receive codec", "track", t.SID(), "codec", receiver.Codec().MimeType)
			p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionResponse{SubscriptionResponse: &pb.SubscriptionResponse{
				TrackSid: t.SID(),
				Err:      pb.SubscriptionError_SE_CODEC_UNSUPPORTED,
			}}})
			p.Unsubscribe(t.SID())
		},
	})
	p.mu.Lock()
	s.down = down
	p.mu.Unlock()

	var sender *webrtc.RTPSender
	var err error
	if shareStream {
		// A new m-section each time, as the reference does here. AddTrack would reuse a section a stopped subscription
		// left behind, which moves that section's track between MediaStreams in the browser.
		var tr *webrtc.RTPTransceiver
		tr, err = p.sub.PC().AddTransceiverFromTrack(down, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err == nil {
			sender = tr.Sender()
		}
	} else {
		sender, err = p.sub.PC().AddTrack(down)
	}
	if err != nil {
		p.mu.Lock()
		delete(p.subscribed, t.SID())
		p.mu.Unlock()
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionResponse{SubscriptionResponse: &pb.SubscriptionResponse{
			TrackSid: t.SID(),
			Err:      pb.SubscriptionError_SE_CODEC_UNSUPPORTED,
		}}})
		return err
	}
	p.mu.Lock()
	s.sender = sender
	p.mu.Unlock()

	receiver.AddDownTrack(down)
	t.AddSubscriber(p.sid, down)
	p.alloc.Add(down)
	metrics.TracksOut.Inc()

	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			down.HandleRTCP(pkts)
		}
	}()

	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscriptionPermissionUpdate{
		SubscriptionPermissionUpdate: &pb.SubscriptionPermissionUpdate{ParticipantSid: owner, TrackSid: t.SID(), Allowed: true},
	}})
	if _, ownerP := p.params.Listener.FindTrack(t.SID()); ownerP != nil {
		ownerP.updateSubscribedQuality(t)
	}
	p.sub.Negotiate(false)
	return nil
}

// sharesStreamIDs reports whether this subscriber gets a publisher's audio and video in one MediaStream. The room has to
// ask for it. Firefox would not pair them, and clients before protocol 10 would read the stream as a track ID.
func (p *Participant) sharesStreamIDs() bool {
	return p.params.SyncStreams && p.params.Client.GetProtocol() > 9 && !strings.HasPrefix(strings.ToLower(p.params.Client.GetBrowser()), "firefox")
}

// senderReportLoop tells subscribers how their tracks' timestamps relate to the publisher's clock.
func (p *Participant) senderReportLoop() {
	t := time.NewTicker(senderReportInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.sendSenderReports(time.Now())
		}
	}
}

// sendSenderReports writes a report and a description for each subscribed track that has anything to report, in compound
// packets small enough to stay under an MTU.
func (p *Participant) sendSenderReports(now time.Time) {
	p.mu.RLock()
	subs := make([]*subscription, 0, len(p.subscribed))
	for _, s := range p.subscribed {
		if s.down != nil && s.sender != nil {
			subs = append(subs, s)
		}
	}
	p.mu.RUnlock()
	if len(subs) == 0 {
		return
	}
	mids := map[*webrtc.RTPSender]string{}
	for _, tr := range p.sub.PC().GetTransceivers() {
		if sender := tr.Sender(); sender != nil {
			mids[sender] = tr.Mid()
		}
	}
	var pkts []rtcp.Packet
	var chunks []rtcp.SourceDescriptionChunk
	flush := func() {
		if len(pkts) == 0 {
			return
		}
		pkts = append(pkts, &rtcp.SourceDescription{Chunks: chunks})
		if err := p.sub.PC().WriteRTCP(pkts); err != nil {
			p.log.Debug("could not write sender reports", "error", err)
		}
		pkts, chunks = nil, nil
	}
	for _, s := range subs {
		sr := s.down.CreateSenderReport(now)
		if sr == nil {
			continue
		}
		chunk := s.down.SourceDescription(mids[s.sender])
		pkts = append(pkts, sr)
		chunks = append(chunks, chunk)
		if len(pkts)*senderReportBatchItems >= senderReportBatch {
			flush()
		}
	}
	flush()
}

// Unsubscribe stops sending a track to this participant.
func (p *Participant) Unsubscribe(trackSID string) {
	p.mu.Lock()
	s := p.subscribed[trackSID]
	delete(p.subscribed, trackSID)
	p.mu.Unlock()
	if s == nil {
		return
	}
	s.track.RemoveSubscriber(p.sid)
	if s.down != nil {
		s.down.Close()
	}
	p.teardownSubscription(s)
}

func (p *Participant) onDownTrackClosed(t *track.Published) {
	p.mu.Lock()
	s := p.subscribed[t.SID()]
	delete(p.subscribed, t.SID())
	p.mu.Unlock()
	if s == nil {
		return
	}
	s.track.RemoveSubscriber(p.sid)
	p.teardownSubscription(s)
}

func (p *Participant) teardownSubscription(s *subscription) {
	metrics.TracksOut.Dec()
	if s.down != nil {
		p.alloc.Remove(s.down)
	}
	if s.sender != nil && !p.IsClosed() {
		if err := p.sub.PC().RemoveTrack(s.sender); err != nil {
			p.log.Debug("could not remove track", "error", err)
		}
		p.sub.Negotiate(false)
	}
	if _, ownerP := p.params.Listener.FindTrack(s.track.SID()); ownerP != nil {
		ownerP.updateSubscribedQuality(s.track)
	}
}

// SubscribedTrackSIDs returns current subscriptions.
func (p *Participant) SubscribedTrackSIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.subscribed))
	for sid := range p.subscribed {
		out = append(out, sid)
	}
	return out
}

func (p *Participant) handleTrackSettings(u *pb.UpdateTrackSettings) {
	for _, sid := range u.GetTrackSids() {
		p.mu.RLock()
		s := p.subscribed[sid]
		p.mu.RUnlock()
		if s == nil || s.down == nil {
			continue
		}
		s.down.SetDisabled(u.GetDisabled())
		s.down.SetPriority(u.GetPriority())
		if u.GetWidth() > 0 || u.GetHeight() > 0 {
			s.down.SetMaxLayer(s.track.LayerForDimensions(u.GetWidth(), u.GetHeight()))
		} else if q := u.GetQuality(); q != pb.VideoQuality_OFF {
			s.down.SetMaxLayer(int32(q))
		} else {
			s.down.SetDisabled(true)
		}
		// Zero says nothing about the frame rate, as on the reference. It does not lift an earlier limit.
		if fps := u.GetFps(); fps > 0 {
			s.down.SetMaxFPS(fps)
		}
		p.alloc.Reallocate()
		if _, ownerP := p.params.Listener.FindTrack(sid); ownerP != nil {
			ownerP.updateSubscribedQuality(s.track)
		}
	}
}

// dynacastState is what the publisher was last told about one track.
type dynacastState struct {
	sent  bool
	layer int32
	timer *time.Timer
}

// updateSubscribedQuality tells the publisher which layers anyone watches, so it can stop encoding the
// rest. Asking for more takes effect at once, because a viewer is waiting. Asking for less waits, so that
// layers do not stop and restart as viewers come and go or windows resize.
func (p *Participant) updateSubscribedQuality(t *track.Published) {
	if t.Kind() != pb.TrackType_VIDEO {
		return
	}
	layer := t.MaxSubscribedLayer()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	state := p.dynacast[t.SID()]
	if state == nil {
		state = &dynacastState{}
		p.dynacast[t.SID()] = state
	}
	upgrade := layer != sfu.LayerNone && (!state.sent || layer > state.layer)
	if upgrade || (state.sent && layer == state.layer) {
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
	}
	if !upgrade {
		if (!state.sent || layer != state.layer) && state.timer == nil {
			state.timer = time.AfterFunc(dynacastDownDelay, func() { p.applyDynacastDown(t) })
		}
		p.mu.Unlock()
		return
	}
	state.sent, state.layer = true, layer
	p.mu.Unlock()
	p.sendSubscribedQuality(t, layer)
}

func (p *Participant) applyDynacastDown(t *track.Published) {
	layer := t.MaxSubscribedLayer()
	p.mu.Lock()
	state := p.dynacast[t.SID()]
	if p.closed || state == nil {
		p.mu.Unlock()
		return
	}
	state.timer = nil
	state.sent, state.layer = true, layer
	p.mu.Unlock()
	p.sendSubscribedQuality(t, layer)
}

func (p *Participant) sendSubscribedQuality(t *track.Published, layer int32) {
	qualities := make([]*pb.SubscribedQuality, 0, sfu.MaxLayers)
	for l := sfu.LayerLow; l <= sfu.LayerHigh; l++ {
		qualities = append(qualities, &pb.SubscribedQuality{Quality: pb.VideoQuality(l), Enabled: l <= layer})
	}
	// Clients match on the short codec name and ignore a mime type.
	codec := strings.TrimPrefix(strings.ToLower(t.Info().GetMimeType()), "video/")
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SubscribedQualityUpdate{
		SubscribedQualityUpdate: &pb.SubscribedQualityUpdate{
			TrackSid:            t.SID(),
			SubscribedQualities: qualities,
			SubscribedCodecs:    []*pb.SubscribedCodec{{Codec: codec, Qualities: qualities}},
		},
	}})
}

// resendSubscribedQualities repeats what the publisher was last told. Applying an answer re-enables
// encodings on the client, so each answer would otherwise undo earlier updates.
func (p *Participant) resendSubscribedQualities() {
	type pending struct {
		t     *track.Published
		layer int32
	}
	var out []pending
	p.mu.RLock()
	for sid, state := range p.dynacast {
		if t := p.published[sid]; t != nil && state.sent {
			out = append(out, pending{t, state.layer})
		}
	}
	p.mu.RUnlock()
	for _, x := range out {
		p.sendSubscribedQuality(x.t, x.layer)
	}
}

func (p *Participant) onPauseChange(d *sfu.DownTrack, paused bool) {
	p.mu.RLock()
	var owner string
	if s := p.subscribed[d.ID()]; s != nil {
		owner = s.track.OwnerSID()
	}
	p.mu.RUnlock()
	if owner == "" {
		return
	}
	state := pb.StreamState_ACTIVE
	if paused {
		state = pb.StreamState_PAUSED
	}
	p.sendStreamState(owner, d.ID(), state)
}

func (p *Participant) sendStreamState(ownerSID, trackSID string, state pb.StreamState) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_StreamStateUpdate{StreamStateUpdate: &pb.StreamStateUpdate{
		StreamStates: []*pb.StreamStateInfo{{ParticipantSid: ownerSID, TrackSid: trackSID, State: state}},
	}}})
}

func (p *Participant) handleSubscriptionPermission(sp *pb.SubscriptionPermission) {
	p.mu.Lock()
	p.allowAll = sp.GetAllParticipants()
	p.allowedBySub = map[string]map[string]bool{}
	for _, tp := range sp.GetTrackPermissions() {
		for _, who := range []string{tp.GetParticipantSid(), tp.GetParticipantIdentity()} {
			if who == "" {
				continue
			}
			set := p.allowedBySub[who]
			if set == nil {
				set = map[string]bool{}
				p.allowedBySub[who] = set
			}
			if tp.GetAllTracks() {
				set["*"] = true
			}
			for _, sid := range tp.GetTrackSids() {
				set[sid] = true
			}
		}
	}
	p.mu.Unlock()
	p.params.Listener.OnPermissionsChanged(p)
}

// AllowsSubscriber reports whether this participant lets a subscriber receive one of its tracks.
func (p *Participant) AllowsSubscriber(subscriber *Participant, trackSID string) bool {
	// A publisher's sharing settings choose among the people in the room, and a recording of the room is not
	// one of them. Starting a recording takes its own permission.
	if subscriber.Recorder() {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.allowAll {
		return true
	}
	for _, who := range []string{subscriber.SID(), subscriber.Identity()} {
		if set := p.allowedBySub[who]; set != nil && (set["*"] || set[trackSID]) {
			return true
		}
	}
	return false
}

// Tracks returns the tracks this participant publishes.
func (p *Participant) Tracks() []*track.Published {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*track.Published, 0, len(p.published))
	for _, t := range p.published {
		out = append(out, t)
	}
	return out
}

// Track returns one published track, or nil.
func (p *Participant) Track(sid string) *track.Published {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.published[sid]
}

func (p *Participant) handleUpdateMetadata(u *pb.UpdateParticipantMetadata) {
	reply := func(reason pb.RequestResponse_Reason, msg string) {
		if u.GetRequestId() == 0 {
			return
		}
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_RequestResponse{RequestResponse: &pb.RequestResponse{
			RequestId: u.GetRequestId(),
			Reason:    reason,
			Message:   msg,
			Request:   &pb.RequestResponse_UpdateMetadata{UpdateMetadata: u},
		}}})
	}
	p.mu.RLock()
	allowed := p.permission.GetCanUpdateMetadata()
	p.mu.RUnlock()
	if !allowed {
		reply(pb.RequestResponse_NOT_ALLOWED, "does not have permission to update own metadata")
		return
	}
	size := len(u.GetMetadata()) + len(u.GetName())
	for k, v := range u.GetAttributes() {
		size += len(k) + len(v)
	}
	if limit := p.params.MaxMetadataBytes; limit > 0 && size > limit {
		reply(pb.RequestResponse_LIMIT_EXCEEDED, "metadata is too large")
		return
	}
	p.UpdateInfo(&u.Metadata, &u.Name, u.GetAttributes(), nil)
	reply(pb.RequestResponse_OK, "")
}

// UpdateInfo changes a participant's visible state. Nil pointers and empty maps leave a field alone. In
// attributes, an empty value deletes the key.
func (p *Participant) UpdateInfo(metadata, name *string, attributes map[string]string, permission *pb.ParticipantPermission) {
	p.mu.Lock()
	if metadata != nil && *metadata != "" {
		p.metadata = *metadata
	}
	if name != nil && *name != "" {
		p.name = *name
	}
	for k, v := range attributes {
		if v == "" {
			delete(p.attributes, k)
		} else {
			p.attributes[k] = v
		}
	}
	revoked, granted, dataRevoked := false, false, false
	if permission != nil {
		revoked = p.permission.GetCanSubscribe() && !permission.GetCanSubscribe()
		granted = !p.permission.GetCanSubscribe() && permission.GetCanSubscribe()
		dataRevoked = p.permission.GetCanPublishData() && !permission.GetCanPublishData()
		p.permission = proto.Clone(permission).(*pb.ParticipantPermission)
		p.applyPermissionToGrants(permission)
	}
	p.mu.Unlock()
	if dataRevoked {
		p.unpublishAllDataTracks(true)
	}
	p.changed()
	if revoked {
		for _, sid := range p.SubscribedTrackSIDs() {
			p.Unsubscribe(sid)
		}
		p.revokeAllDataSubscriptions()
	}
	if granted {
		p.ReconcileDataSubscriptions()
	}
}

func (p *Participant) applyPermissionToGrants(perm *pb.ParticipantPermission) {
	v := p.params.Grants.Video
	pub, sub, data, meta := perm.GetCanPublish(), perm.GetCanSubscribe(), perm.GetCanPublishData(), perm.GetCanUpdateMetadata()
	v.CanPublish, v.CanSubscribe, v.CanPublishData, v.CanUpdateOwnMetadata = &pub, &sub, &data, &meta
	v.CanPublishSources = v.CanPublishSources[:0]
	for _, s := range perm.GetCanPublishSources() {
		v.CanPublishSources = append(v.CanPublishSources, sourceName(s))
	}
}

func (p *Participant) handleSyncState(s *pb.SyncState) {
	// A client that believes it publishes a data track this server does not have cannot be repaired by a resume. Its other
	// tracks have to be published again from the start.
	if sid, missing := p.missingDataTrack(s.GetPublishDataTracks()); missing {
		p.log.Warn("resuming client publishes an unknown data track, starting a full reconnect", "track", sid)
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Leave{Leave: &pb.LeaveRequest{
			CanReconnect: true,
			Reason:       pb.DisconnectReason_STATE_MISMATCH,
			Action:       pb.LeaveRequest_RECONNECT,
		}}})
		p.Close(false, pb.DisconnectReason_STATE_MISMATCH)
		return
	}
	// Restore what the client believes it is subscribed to, if it differs after a resume.
	if sub := s.GetSubscription(); sub != nil {
		p.handleSubscription(sub)
	}
	if ids := s.GetTrackSidsDisabled(); len(ids) > 0 {
		p.handleTrackSettings(&pb.UpdateTrackSettings{TrackSids: ids, Disabled: true})
	}
	// A subscriber answer the client received before the signal connection dropped may never have
	// reached us. Replaying it completes a negotiation that would otherwise hang. With one connection this field is this
	// server's own answer, and there is nothing to replay.
	if answer := s.GetAnswer(); !p.params.SinglePC && answer != nil && answer.GetType() == "answer" && answer.GetId() == p.sub.CurrentOfferID() {
		if err := p.sub.HandleAnswer(answer); err != nil {
			p.log.Debug("could not replay answer from sync state", "error", err)
		}
	}
	for _, r := range s.GetDatachannelReceiveStates() {
		p.log.Debug("client data receive state", "from", r.GetPublisherSid(), "seq", r.GetLastSeq())
	}
}

func (p *Participant) handleSimulate(s *pb.SimulateScenario) {
	if !p.params.Development {
		p.log.Warn("ignored simulate request: not in development mode")
		return
	}
	switch s.GetScenario().(type) {
	case *pb.SimulateScenario_ServerLeave:
		p.Close(true, pb.DisconnectReason_SERVER_SHUTDOWN)
	case *pb.SimulateScenario_LeaveRequestFullReconnect:
		p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Leave{Leave: &pb.LeaveRequest{
			Reason: pb.DisconnectReason_SERVER_SHUTDOWN, Action: pb.LeaveRequest_RECONNECT, CanReconnect: true,
		}}})
	case *pb.SimulateScenario_NodeFailure:
		p.sinkMu.Lock()
		sink := p.sink
		p.sinkMu.Unlock()
		if sink != nil {
			sink.Close("simulated node failure")
		}
	default:
		p.log.Debug("unsupported simulate scenario")
	}
}

func (p *Participant) onPublisherDataChannel(dc *webrtc.DataChannel) {
	if p.params.SinglePC {
		// The client's own channels carry what this server sends, too.
		switch dc.Label() {
		case reliableChannel:
			p.reliable.attach(dc)
		case lossyChannel:
			p.lossy.attach(dc)
		case datatrack.ChannelLabel:
			p.dataTrack.attach(dc)
		}
	}
	if dc.Label() == datatrack.ChannelLabel {
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { p.onDataTrackMessage(msg.Data) })
		return
	}
	kind := pb.DataPacket_RELIABLE
	if dc.Label() == lossyChannel {
		kind = pb.DataPacket_LOSSY
	} else if dc.Label() != reliableChannel {
		return
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { p.onDataMessage(kind, msg.Data) })
}

func (p *Participant) onDataMessage(kind pb.DataPacket_Kind, data []byte) {
	if limit := p.params.MaxDataPacketBytes; limit > 0 && len(data) > limit {
		p.log.Debug("dropping oversized data packet", "bytes", len(data))
		return
	}
	dp := &pb.DataPacket{}
	if err := proto.Unmarshal(data, dp); err != nil {
		p.log.Debug("bad data packet", "error", err)
		return
	}
	p.mu.RLock()
	allowed := p.permission.GetCanPublishData()
	p.mu.RUnlock()
	if !allowed {
		return
	}
	seq := dp.GetSequence()
	reliable := kind == pb.DataPacket_RELIABLE && seq != 0
	// A client resends unacknowledged packets after a resume, so duplicates are expected. Those cost nothing here.
	if reliable && seq <= p.lastReliableSeq.Load() {
		return
	}
	// This is about the room having to pass it to everyone. A dropped packet is not recorded, so going past the limit does not
	// turn into a false claim of having received it when the client asks after a resume.
	if !p.allowData(len(data)) {
		metrics.DataPacketsDropped.WithLabelValues(kind.String()).Inc()
		p.log.Debug("dropping a data packet over the rate limit", "bytes", len(data))
		return
	}
	if reliable {
		p.lastReliableSeq.Store(seq)
	}
	p.params.Listener.OnDataPacket(p, kind, dp)
}

// SendData delivers raw packet bytes on the matching data channel.
func (p *Participant) SendData(kind pb.DataPacket_Kind, data []byte) {
	if kind == pb.DataPacket_LOSSY {
		p.lossy.send(data, false)
		return
	}
	p.reliable.send(data, true)
}

// SendSpeakers relays active speaker changes.
func (p *Participant) SendSpeakers(speakers []*pb.SpeakerInfo) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_SpeakersChanged{SpeakersChanged: &pb.SpeakersChanged{Speakers: speakers}}})
}

// SendParticipantUpdate relays changes to other participants.
func (p *Participant) SendParticipantUpdate(infos []*pb.ParticipantInfo) {
	if len(infos) == 0 {
		return
	}
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_Update{Update: &pb.ParticipantUpdate{Participants: infos}}})
}

// SendRoomUpdate relays room metadata and counts.
func (p *Participant) SendRoomUpdate(room *pb.Room) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_RoomUpdate{RoomUpdate: &pb.RoomUpdate{Room: room}}})
}

// SendConnectionQuality relays connection quality scores.
func (p *Participant) SendConnectionQuality(infos []*pb.ConnectionQualityInfo) {
	p.Send(&pb.SignalResponse{Message: &pb.SignalResponse_ConnectionQuality{ConnectionQuality: &pb.ConnectionQualityUpdate{Updates: infos}}})
}

// SetConnectionQuality records the latest score, for monitoring.
func (p *Participant) SetConnectionQuality(score float32, quality pb.ConnectionQuality) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.qualityScore, p.quality = score, quality
}

// ConnectionQuality returns the latest score and its name. It is "UNKNOWN" until first measured.
func (p *Participant) ConnectionQuality() (float32, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.qualityScore == 0 {
		return 0, "UNKNOWN"
	}
	return p.qualityScore, p.quality.String()
}

// Stats describes this participant's media at a point in time.
type Stats struct {
	PublishedBitrate  int64
	PublishLost       uint64
	PublishPackets    uint64
	SubscribedPackets uint64
	SubscribedBytes   uint64
	SubscribedBitrate int64
	SubscribeLoss     float32
	SubscribeRTT      time.Duration
	SubscribeJitter   time.Duration
	BandwidthEstimate int64
	Nacks             uint64
	Plis              uint64
}

// Stats collects current media counters.
func (p *Participant) Stats() Stats {
	var out Stats
	if p.alloc != nil {
		out.BandwidthEstimate = p.alloc.Limit()
	}
	p.mu.RLock()
	published := make([]*track.Published, 0, len(p.published))
	for _, t := range p.published {
		published = append(published, t)
	}
	subs := make([]*subscription, 0, len(p.subscribed))
	for _, s := range p.subscribed {
		subs = append(subs, s)
	}
	p.mu.RUnlock()

	for _, t := range published {
		if r := t.Receiver(); r != nil {
			s := r.Stats()
			out.PublishedBitrate += s.Bitrate
			out.PublishLost += s.Lost
			out.PublishPackets += s.Packets
		}
	}
	for _, s := range subs {
		if s.down == nil {
			continue
		}
		d := s.down.Stats()
		out.SubscribedPackets += d.Packets
		out.SubscribedBytes += d.Bytes
		out.Nacks += d.NACKs
		out.Plis += d.PLIs
		out.SubscribeLoss = max(out.SubscribeLoss, d.Loss)
		out.SubscribeJitter = max(out.SubscribeJitter, d.Jitter)
		out.SubscribeRTT = max(out.SubscribeRTT, d.RTT)
		if layer := s.down.Current(); layer != sfu.LayerNone {
			out.SubscribedBitrate += s.down.Receiver().LayerBitrate(layer)
		}
	}
	return out
}

// dataSender writes to a data channel, holding packets until the channel opens.
type dataSender struct {
	mu      sync.Mutex
	dc      *webrtc.DataChannel
	open    bool
	pending [][]byte
}

// newDataSender returns a sender for dc, or with nil, one that holds packets until attach gives it a channel.
func newDataSender(dc *webrtc.DataChannel) *dataSender {
	s := &dataSender{}
	if dc != nil {
		s.attach(dc)
	}
	return s
}

// attach gives the sender its channel. Pion runs an open handler at once if the channel is already open.
func (s *dataSender) attach(dc *webrtc.DataChannel) {
	s.mu.Lock()
	s.dc = dc
	s.open = false
	s.mu.Unlock()
	dc.OnOpen(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.open = true
		for _, data := range s.pending {
			_ = dc.Send(data)
		}
		s.pending = nil
	})
}

// sendLossy sends data that is worth nothing if it is late. It is dropped if the channel is not open, or too much is already
// waiting to go out.
func (s *dataSender) sendLossy(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open && s.dc.BufferedAmount() <= dataTrackBuffer {
		_ = s.dc.Send(data)
	}
}

func (s *dataSender) send(data []byte, holdUntilOpen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		_ = s.dc.Send(data)
		return
	}
	if holdUntilOpen && len(s.pending) < maxPendingData {
		s.pending = append(s.pending, data)
	}
}
