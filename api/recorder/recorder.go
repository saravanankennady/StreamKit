// Package recorder persists what happens on this node: room history, the event log, webhook deliveries,
// usage, and this node's heartbeat.
package recorder

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

const (
	queueSize         = 10000
	usageInterval     = 10 * time.Second
	heartbeatInterval = 10 * time.Second
	purgeInterval     = time.Hour
)

// Recorder writes room events to the database, off the media path.
type Recorder struct {
	store      *store.Store
	monitor    *monitor.Monitor
	rooms      *room.Manager
	dispatcher *webhook.Dispatcher
	log        *slog.Logger
	cfg        *config.Config
	queue      chan room.Event

	usageMu sync.Mutex
	last    map[string]usageSnapshot
	// failures counts sessions that never connected media, per project, since the last usage write.
	failures map[string]int
	started  time.Time
}

type usageSnapshot struct {
	bytesIn, bytesOut, packetsIn, packetsOut, lost uint64
}

// New creates a Recorder and subscribes it to room events.
func New(s *store.Store, m *monitor.Monitor, rooms *room.Manager, d *webhook.Dispatcher, cfg *config.Config, log *slog.Logger) *Recorder {
	r := &Recorder{
		store: s, monitor: m, rooms: rooms, dispatcher: d, cfg: cfg, log: log,
		queue:    make(chan room.Event, queueSize),
		last:     map[string]usageSnapshot{},
		failures: map[string]int{},
		started:  time.Now(),
	}
	rooms.Subscribe(func(e room.Event) {
		// A slow database must never delay media. Better to lose a log row than a call.
		select {
		case r.queue <- e:
		default:
			log.Warn("event queue is full, dropping event", "type", string(e.Type))
		}
	})
	return r
}

// Run processes events and periodic work until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	usage := time.NewTicker(usageInterval)
	defer usage.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	purge := time.NewTicker(purgeInterval)
	defer purge.Stop()
	r.heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			r.drain()
			return
		case e := <-r.queue:
			r.handle(ctx, e)
		case <-usage.C:
			r.recordUsage(ctx)
		case <-heartbeat.C:
			r.heartbeat(ctx)
		case <-purge.C:
			if err := r.store.PurgeOld(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("could not purge old rows", "error", err)
			}
		}
	}
}

// drain saves events already queued at shutdown, such as rooms closing.
func (r *Recorder) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		select {
		case e := <-r.queue:
			r.handle(ctx, e)
		default:
			return
		}
	}
}

func (r *Recorder) handle(ctx context.Context, e room.Event) {
	roomSID, roomName := e.Room.GetSid(), e.Room.GetName()
	var err error
	switch e.Type {
	case room.EventRoomStarted:
		err = r.store.RoomStarted(ctx, store.RoomRecord{
			SID: roomSID, ProjectID: e.ProjectID, Name: roomName, NodeID: r.rooms.NodeID(),
			Metadata: e.Room.GetMetadata(), MaxParticipants: int(e.Room.GetMaxParticipants()), CreatedAt: e.At,
		})
	case room.EventRoomFinished:
		err = r.store.RoomFinished(ctx, roomSID, e.At)
	case room.EventParticipantJoined:
		err = r.store.ParticipantJoined(ctx, e.Participant.GetSid(), roomSID, e.ProjectID, e.Participant.GetIdentity(),
			e.Participant.GetName(), e.Participant.GetKind().String(), e.At, int(e.Room.GetNumParticipants()))
	case room.EventParticipantLeft, room.EventParticipantConnectionAborted:
		err = r.store.ParticipantLeft(ctx, e.Participant.GetSid(), e.Participant.GetDisconnectReason().String(), e.At)
		if e.Type == room.EventParticipantConnectionAborted {
			r.usageMu.Lock()
			r.failures[e.ProjectID]++
			r.usageMu.Unlock()
		}
	case room.EventTrackPublished:
		err = r.store.TrackPublished(ctx, e.Track.GetSid(), e.Participant.GetSid(), roomSID, e.ProjectID,
			e.Track.GetType().String(), e.Track.GetSource().String(), e.Track.GetMimeType(), e.At)
	case room.EventTrackUnpublished:
		err = r.store.TrackUnpublished(ctx, e.Track.GetSid(), e.At)
	}
	if err != nil && ctx.Err() == nil {
		r.log.Warn("could not save room event", "type", string(e.Type), "error", err)
	}

	row := store.Event{
		Category: categoryOf(e.Type), Level: levelOf(e), Type: string(e.Type), Message: messageOf(e),
		RoomName: roomName, RoomSID: roomSID, ParticipantIdentity: e.Participant.GetIdentity(),
		TrackSID: e.Track.GetSid(), CreatedAt: e.At,
	}
	project := e.ProjectID
	row.ProjectID = &project
	if err := r.store.InsertEvents(ctx, []store.Event{row}); err != nil && ctx.Err() == nil {
		r.log.Warn("could not save event", "error", err)
	}
	r.enqueueWebhooks(ctx, e)
}

func categoryOf(t room.EventType) string {
	switch t {
	case room.EventRoomStarted, room.EventRoomFinished:
		return store.CategoryRoom
	case room.EventTrackPublished, room.EventTrackUnpublished:
		return store.CategoryTrack
	}
	return store.CategoryParticipant
}

func levelOf(e room.Event) string {
	if e.Type == room.EventParticipantConnectionAborted {
		return "warn"
	}
	return "info"
}

func messageOf(e room.Event) string {
	who := e.Participant.GetIdentity()
	switch e.Type {
	case room.EventRoomStarted:
		return "Room started"
	case room.EventRoomFinished:
		return "Room finished"
	case room.EventParticipantJoined:
		return who + " joined"
	case room.EventParticipantLeft:
		return who + " left: " + e.Participant.GetDisconnectReason().String()
	case room.EventParticipantConnectionAborted:
		return who + " could not establish a media connection"
	case room.EventTrackPublished:
		return who + " published " + e.Track.GetSource().String()
	case room.EventTrackUnpublished:
		return who + " unpublished " + e.Track.GetSource().String()
	}
	return string(e.Type)
}

func (r *Recorder) enqueueWebhooks(ctx context.Context, e room.Event) {
	hooks, err := r.store.WebhooksForEvent(ctx, e.ProjectID, string(e.Type))
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("could not look up webhooks", "error", err)
		}
		return
	}
	if len(hooks) == 0 {
		return
	}
	body, err := EncodeEvent(e)
	if err != nil {
		r.log.Warn("could not encode webhook event", "error", err)
		return
	}
	for _, hook := range hooks {
		if _, err := r.store.EnqueueDelivery(ctx, hook.ID, string(e.Type), body); err != nil && ctx.Err() == nil {
			r.log.Warn("could not queue webhook delivery", "error", err)
		}
	}
	r.dispatcher.Notify()
}

// EncodeEvent builds a webhook body. It uses lowerCamelCase names and omits defaults, unlike the room API:
// webhook receivers for this protocol expect that form.
func EncodeEvent(e room.Event) ([]byte, error) {
	event := &pb.WebhookEvent{
		Id:        participant.NewID("EV_"),
		Event:     string(e.Type),
		CreatedAt: e.At.Unix(),
		Room:      e.Room,
	}
	switch e.Type {
	case room.EventTrackPublished, room.EventTrackUnpublished:
		// Track events carry only identifying fields of the room and participant.
		event.Room = &pb.Room{Sid: e.Room.GetSid(), Name: e.Room.GetName()}
		event.Participant = &pb.ParticipantInfo{Sid: e.Participant.GetSid(), Identity: e.Participant.GetIdentity()}
		event.Track = e.Track
	case room.EventParticipantJoined, room.EventParticipantLeft, room.EventParticipantConnectionAborted:
		event.Participant = proto.Clone(e.Participant).(*pb.ParticipantInfo)
	}
	return protojson.Marshal(event)
}

func (r *Recorder) recordUsage(ctx context.Context) {
	now := time.Now()
	seconds := int64(usageInterval / time.Second)
	r.usageMu.Lock()
	failures := r.failures
	r.failures = map[string]int{}
	r.usageMu.Unlock()
	projects := r.monitor.ProjectIDs()
	// A project can have only failures, with nothing ever having connected.
	for id := range failures {
		if !slices.Contains(projects, id) {
			projects = append(projects, id)
		}
	}
	for _, id := range projects {
		snap := r.monitor.Project(id)
		bytesIn, bytesOut, packetsIn, packetsOut, lost := r.monitor.Totals(id)
		r.usageMu.Lock()
		prev := r.last[id]
		r.last[id] = usageSnapshot{bytesIn, bytesOut, packetsIn, packetsOut, lost}
		r.usageMu.Unlock()
		if snap.ActiveRooms == 0 && bytesIn == prev.bytesIn && bytesOut == prev.bytesOut && failures[id] == 0 {
			continue
		}
		var recording int64
		for _, room := range snap.Rooms {
			if room.ActiveRecording {
				recording += seconds
			}
		}
		delta := store.UsageDelta{
			ProjectID:          id,
			Bucket:             now,
			ParticipantSeconds: int64(snap.Participants) * seconds,
			RoomSeconds:        int64(snap.ActiveRooms) * seconds,
			AudioTrackSeconds:  int64(snap.AudioStreams) * seconds,
			VideoTrackSeconds:  int64(snap.VideoStreams) * seconds,
			RecordingSeconds:   recording,
			BytesIn:            int64(bytesIn - prev.bytesIn),
			BytesOut:           int64(bytesOut - prev.bytesOut),
			PacketsIn:          int64(packetsIn - prev.packetsIn),
			PacketsOut:         int64(packetsOut - prev.packetsOut),
			PacketsLost:        int64(lost - prev.lost),
			ConnectionFailures: failures[id],
			PeakParticipants:   snap.Participants,
			PeakRooms:          snap.ActiveRooms,
			AvgRTTMs:           float32(snap.AvgRTTMs),
		}
		if err := r.store.AddUsage(ctx, delta); err != nil && ctx.Err() == nil {
			r.log.Warn("could not save usage", "error", err)
		}
	}
}

func (r *Recorder) heartbeat(ctx context.Context) {
	node := r.monitor.Node()
	totals := struct {
		monitor.NodeStats
		ActiveRooms  int `json:"active_rooms"`
		Participants int `json:"participants"`
	}{NodeStats: node}
	for _, x := range r.rooms.AllRooms() {
		if s := x.State(); s != room.StateClosed && s != room.StateClosing {
			totals.ActiveRooms++
			totals.Participants += len(x.Participants())
		}
	}
	stats, err := json.Marshal(totals)
	if err != nil {
		return
	}
	draining, err := r.store.HeartbeatServer(ctx, store.ServerNode{
		NodeID:    r.rooms.NodeID(),
		Region:    r.cfg.Region,
		Version:   config.Version,
		Address:   r.cfg.ControlPlane.PublicWSURL,
		StartedAt: r.started,
		Stats:     stats,
	})
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("heartbeat failed", "error", err)
		}
		return
	}
	r.rooms.SetDraining(draining)
}
