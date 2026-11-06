package monitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/routing"
)

// Cluster keeps this node registered with the others and shares live stats, so any node can answer for
// the whole cluster. It does nothing beyond what a room's own routing needs on a single node.
type Cluster struct {
	monitor *Monitor
	rooms   *room.Manager
	redis   *routing.Redis
	url     string
	region  string
	log     *slog.Logger

	mu    sync.RWMutex
	nodes []routing.Node
}

// NewCluster creates a Cluster. url is how other nodes reach this one.
func NewCluster(m *Monitor, rooms *room.Manager, redis *routing.Redis, url, region string, log *slog.Logger) *Cluster {
	c := &Cluster{monitor: m, rooms: rooms, redis: redis, url: url, region: region, log: log}
	rooms.Subscribe(func(e room.Event) {
		if e.Type != room.EventRoomFinished {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := redis.Release(ctx, e.ProjectID, e.Room.GetName()); err != nil {
			log.Warn("could not release room", "room", e.Room.GetName(), "error", err)
		}
	})
	return c
}

// Run keeps registration and shared stats fresh until ctx ends.
func (c *Cluster) Run(ctx context.Context) {
	c.refresh(ctx)
	refresh := time.NewTicker(routing.RefreshInterval)
	defer refresh.Stop()
	publish := time.NewTicker(time.Second)
	defer publish.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			c.refresh(ctx)
		case <-publish.C:
			c.publish(ctx)
		}
	}
}

func (c *Cluster) refresh(ctx context.Context) {
	var refs []routing.RoomRef
	var participants int
	for _, r := range c.rooms.AllRooms() {
		if s := r.State(); s == room.StateClosed || s == room.StateClosing {
			continue
		}
		refs = append(refs, routing.RoomRef{Project: r.ProjectID(), Name: r.Name()})
		participants += len(r.Participants())
	}
	node := c.monitor.Node()
	self := routing.Node{
		ID: c.rooms.NodeID(), URL: c.url, Region: c.region, Rooms: len(refs), Participants: participants,
		CPUPercent: node.CPUPercent, Draining: c.rooms.Draining(),
	}
	lost, err := c.redis.Refresh(ctx, self, refs)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("could not refresh cluster registration", "error", err)
		}
		return
	}
	// Another node took these while this one was out of touch. Two copies of a room would each have half its people, so
	// this one closes, and its people connect again and reach the other.
	for _, ref := range lost {
		c.log.Warn("another node holds this room now, so closing the copy here", "room", ref.Name, "project", ref.Project)
		c.rooms.Evict(ref.Project, ref.Name)
	}
	nodes, err := c.redis.Nodes(ctx)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.nodes = nodes
	c.mu.Unlock()
}

func (c *Cluster) publish(ctx context.Context) {
	for _, id := range c.monitor.ProjectIDs() {
		data, err := json.Marshal(c.monitor.Project(id))
		if err != nil {
			continue
		}
		if err := c.redis.PublishSnapshot(ctx, id, data); err != nil {
			if ctx.Err() == nil {
				c.log.Debug("could not publish stats", "error", err)
			}
			return
		}
	}
}

// Peers returns other nodes' latest stats for a project.
func (c *Cluster) Peers(projectID string) []*ProjectSnapshot {
	c.mu.RLock()
	nodes := c.nodes
	c.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := c.redis.Snapshots(ctx, projectID, nodes)
	if err != nil {
		c.log.Debug("could not read peer stats", "error", err)
		return nil
	}
	out := make([]*ProjectSnapshot, 0, len(raw))
	for _, data := range raw {
		var s ProjectSnapshot
		if json.Unmarshal(data, &s) == nil {
			out = append(out, &s)
		}
	}
	return out
}

// RoomsOf returns other nodes' open rooms in a project, for listing.
func (c *Cluster) RoomsOf(projectID string) []*pb.Room {
	var out []*pb.Room
	for _, snap := range c.Peers(projectID) {
		for _, r := range snap.Rooms {
			out = append(out, &pb.Room{
				Sid:              r.SID,
				Name:             r.Name,
				Metadata:         r.Metadata,
				NumParticipants:  uint32(r.Participants),
				NumPublishers:    uint32(r.Publishers),
				MaxParticipants:  r.MaxParticipants,
				CreationTime:     r.CreatedAt,
				EmptyTimeout:     r.EmptyTimeout,
				DepartureTimeout: r.DepartureTimeout,
				ActiveRecording:  r.ActiveRecording,
			})
		}
	}
	return out
}

// Merge adds other nodes' figures to a snapshot. Rates and counts add up. Latency and jitter become an
// average weighted by participants, and loss the worst seen, matching what one node reports.
func Merge(local *ProjectSnapshot, peers []*ProjectSnapshot) *ProjectSnapshot {
	if len(peers) == 0 {
		return local
	}
	out := *local
	out.Rooms = append([]RoomStat{}, local.Rooms...)
	out.ParticipantStats = append([]ParticipantStat{}, local.ParticipantStats...)
	rtt := out.AvgRTTMs * float64(out.Participants)
	jitter := out.AvgJitterMs * float64(out.Participants)
	for _, p := range peers {
		out.ActiveRooms += p.ActiveRooms
		out.Participants += p.Participants
		out.Connections += p.Connections
		out.AudioStreams += p.AudioStreams
		out.VideoStreams += p.VideoStreams
		out.Subscriptions += p.Subscriptions
		out.BitrateIn += p.BitrateIn
		out.BitrateOut += p.BitrateOut
		out.PacketsInRate += p.PacketsInRate
		out.PacketsOutRate += p.PacketsOutRate
		out.PacketLossRatio = max(out.PacketLossRatio, p.PacketLossRatio)
		rtt += p.AvgRTTMs * float64(p.Participants)
		jitter += p.AvgJitterMs * float64(p.Participants)
		out.Rooms = append(out.Rooms, p.Rooms...)
		out.ParticipantStats = append(out.ParticipantStats, p.ParticipantStats...)
	}
	if out.Participants > 0 {
		out.AvgRTTMs = rtt / float64(out.Participants)
		out.AvgJitterMs = jitter / float64(out.Participants)
	}
	return &out
}
