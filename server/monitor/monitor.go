// Package monitor turns the live state of rooms into per-project snapshots: what the dashboard shows and
// what usage accounting is based on. Every figure here is measured from real media; none is estimated
// or filled in.
package monitor

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/sfu"
)

const interval = time.Second

// TrackStat is one published track.
type TrackStat struct {
	SID         string `json:"sid"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	MimeType    string `json:"mime_type"`
	Muted       bool   `json:"muted"`
	Simulcast   bool   `json:"simulcast"`
	Width       uint32 `json:"width"`
	Height      uint32 `json:"height"`
	FPS         int32  `json:"fps"`
	BitrateIn   int64  `json:"bitrate_in"`
	Layers      int    `json:"layers"`
	PacketsIn   uint64 `json:"packets_in"`
	PacketsLost uint64 `json:"packets_lost"`
	Subscribers int    `json:"subscribers"`
}

// ParticipantStat is one member of a room.
type ParticipantStat struct {
	SID             string                    `json:"sid"`
	Identity        string                    `json:"identity"`
	Name            string                    `json:"name"`
	Room            string                    `json:"room"`
	State           string                    `json:"state"`
	Kind            string                    `json:"kind"`
	JoinedAt        int64                     `json:"joined_at"`
	DurationSeconds int64                     `json:"duration_seconds"`
	Quality         string                    `json:"quality"`
	QualityScore    float32                   `json:"quality_score"`
	Tracks          []TrackStat               `json:"tracks"`
	SubscribedCount int                       `json:"subscribed_count"`
	HasAudio        bool                      `json:"has_audio"`
	HasVideo        bool                      `json:"has_video"`
	AudioMuted      bool                      `json:"audio_muted"`
	VideoMuted      bool                      `json:"video_muted"`
	BitrateIn       int64                     `json:"bitrate_in"`
	BitrateOut      int64                     `json:"bitrate_out"`
	BandwidthEst    int64                     `json:"bandwidth_estimate"`
	RTTMs           float64                   `json:"rtt_ms"`
	JitterMs        float64                   `json:"jitter_ms"`
	LossRatio       float64                   `json:"loss_ratio"`
	PacketsIn       uint64                    `json:"packets_in"`
	PacketsOut      uint64                    `json:"packets_out"`
	Permission      *pb.ParticipantPermission `json:"permission,omitempty"`
}

// RoomStat is one open room.
type RoomStat struct {
	SID              string `json:"sid"`
	Name             string `json:"name"`
	State            string `json:"state"`
	Metadata         string `json:"metadata"`
	Participants     int    `json:"participants"`
	MaxParticipants  uint32 `json:"max_participants"`
	Publishers       int    `json:"publishers"`
	AudioStreams     int    `json:"audio_streams"`
	VideoStreams     int    `json:"video_streams"`
	CreatedAt        int64  `json:"created_at"`
	DurationSeconds  int64  `json:"duration_seconds"`
	BitrateIn        int64  `json:"bitrate_in"`
	BitrateOut       int64  `json:"bitrate_out"`
	EmptyTimeout     uint32 `json:"empty_timeout"`
	DepartureTimeout uint32 `json:"departure_timeout"`
	ActiveRecording  bool   `json:"active_recording"`
}

// ProjectSnapshot is everything live in one project.
type ProjectSnapshot struct {
	At               time.Time         `json:"at"`
	ActiveRooms      int               `json:"active_rooms"`
	Participants     int               `json:"participants"`
	Connections      int               `json:"connections"`
	AudioStreams     int               `json:"audio_streams"`
	VideoStreams     int               `json:"video_streams"`
	Subscriptions    int               `json:"subscriptions"`
	BitrateIn        int64             `json:"bitrate_in"`
	BitrateOut       int64             `json:"bitrate_out"`
	PacketsInRate    int64             `json:"packets_in_rate"`
	PacketsOutRate   int64             `json:"packets_out_rate"`
	PacketLossRatio  float64           `json:"packet_loss_ratio"`
	AvgRTTMs         float64           `json:"avg_rtt_ms"`
	AvgJitterMs      float64           `json:"avg_jitter_ms"`
	Rooms            []RoomStat        `json:"rooms"`
	ParticipantStats []ParticipantStat `json:"participant_stats"`
	Node             NodeStats         `json:"node"`

	// Totals at this sample, for computing rates from the next one.
	totalPacketsIn, totalPacketsOut uint64
}

// NodeStats describes the machine this server runs on.
type NodeStats struct {
	NodeID        string  `json:"node_id"`
	Hostname      string  `json:"hostname"`
	CPUPercent    float64 `json:"cpu_percent"`
	ProcessCPU    float64 `json:"process_cpu_percent"`
	NumCPU        int     `json:"num_cpu"`
	MemoryUsed    uint64  `json:"memory_used"`
	MemoryTotal   uint64  `json:"memory_total"`
	ProcessMemory uint64  `json:"process_memory"`
	Goroutines    int     `json:"goroutines"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	Draining      bool    `json:"draining"`
	Healthy       bool    `json:"healthy"`
}

// counters are one participant's totals at one sample.
type counters struct {
	bytesIn, bytesOut, packetsIn, packetsOut uint64
}

// Monitor samples rooms once a second.
type Monitor struct {
	rooms   *room.Manager
	started time.Time
	proc    *process.Process
	host    string

	mu       sync.RWMutex
	projects map[string]*ProjectSnapshot
	node     NodeStats
	previous map[string]counters
	lifetime map[string]*lifetimeTotals
	lastTick time.Time

	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}

	peers func(projectID string) []*ProjectSnapshot
}

// lifetimeTotals never goes down when a participant leaves, unlike summing live participants.
type lifetimeTotals struct {
	bytesIn, bytesOut, packetsIn, packetsOut, lost uint64
	seen                                           map[string]counters
	seenLost                                       map[string]uint64
}

// New creates a Monitor. Call Run to start it.
func New(rooms *room.Manager) *Monitor {
	m := &Monitor{
		rooms:    rooms,
		started:  time.Now(),
		projects: map[string]*ProjectSnapshot{},
		previous: map[string]counters{},
		lifetime: map[string]*lifetimeTotals{},
		subs:     map[chan struct{}]struct{}{},
	}
	m.host, _ = os.Hostname()
	if p, err := process.NewProcess(int32(os.Getpid())); err == nil {
		m.proc = p
	}
	return m
}

// Run samples until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	// The first CPU reading only sets a baseline.
	_, _ = cpu.Percent(0, false)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.sample(now)
			m.notify()
		}
	}
}

// Subscribe returns a channel signalled after each sample, and a function to stop.
func (m *Monitor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()
	return ch, func() {
		m.subsMu.Lock()
		delete(m.subs, ch)
		m.subsMu.Unlock()
	}
}

func (m *Monitor) notify() {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SetPeers installs where to get other nodes' stats, on a cluster.
func (m *Monitor) SetPeers(fn func(projectID string) []*ProjectSnapshot) { m.peers = fn }

// View returns a project's snapshot across the whole cluster. On a single node it equals Project.
func (m *Monitor) View(id string) *ProjectSnapshot {
	local := m.Project(id)
	if m.peers == nil {
		return local
	}
	return Merge(local, m.peers(id))
}

// Project returns the latest snapshot for a project on this node. It is never nil.
func (m *Monitor) Project(id string) *ProjectSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s := m.projects[id]; s != nil {
		return s
	}
	return &ProjectSnapshot{At: time.Now(), Rooms: []RoomStat{}, ParticipantStats: []ParticipantStat{}, Node: m.node}
}

// Node returns the latest machine stats.
func (m *Monitor) Node() NodeStats {
	m.mu.RLock()
	node := m.node
	m.mu.RUnlock()
	// Before the first sample, measure now rather than report an empty, unhealthy-looking node.
	if node.NodeID == "" {
		return m.nodeStats()
	}
	return node
}

// Totals returns lifetime counters for a project, for computing usage over an interval.
func (m *Monitor) Totals(id string) (bytesIn, bytesOut, packetsIn, packetsOut, lost uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t := m.lifetime[id]; t != nil {
		return t.bytesIn, t.bytesOut, t.packetsIn, t.packetsOut, t.lost
	}
	return 0, 0, 0, 0, 0
}

// ProjectIDs lists projects with data on this node.
func (m *Monitor) ProjectIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.lifetime))
	for id := range m.lifetime {
		out = append(out, id)
	}
	return out
}

func (m *Monitor) sample(now time.Time) {
	node := m.nodeStats()
	seconds := now.Sub(m.lastTick).Seconds()
	if m.lastTick.IsZero() || seconds <= 0 {
		seconds = interval.Seconds()
	}
	m.lastTick = now

	projects := map[string]*ProjectSnapshot{}
	next := make(map[string]counters, len(m.previous))

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.rooms.AllRooms() {
		state := r.State()
		if state == room.StateClosed || state == room.StateClosing {
			continue
		}
		snap := projects[r.ProjectID()]
		if snap == nil {
			snap = &ProjectSnapshot{At: now, Rooms: []RoomStat{}, ParticipantStats: []ParticipantStat{}, Node: node}
			projects[r.ProjectID()] = snap
		}
		totals := m.lifetime[r.ProjectID()]
		if totals == nil {
			totals = &lifetimeTotals{seen: map[string]counters{}, seenLost: map[string]uint64{}}
			m.lifetime[r.ProjectID()] = totals
		}
		info := r.ToProto()
		stat := RoomStat{
			SID:              r.SID(),
			Name:             r.Name(),
			State:            string(state),
			Metadata:         info.GetMetadata(),
			MaxParticipants:  info.GetMaxParticipants(),
			CreatedAt:        info.GetCreationTime(),
			DurationSeconds:  now.Unix() - info.GetCreationTime(),
			EmptyTimeout:     info.GetEmptyTimeout(),
			DepartureTimeout: info.GetDepartureTimeout(),
			ActiveRecording:  info.GetActiveRecording(),
		}
		snap.ActiveRooms++
		for _, p := range r.Participants() {
			ps := m.participantStat(p, r.Name(), now, seconds, next, totals)
			if !p.Hidden() {
				stat.Participants++
			}
			if len(ps.Tracks) > 0 {
				stat.Publishers++
			}
			for _, t := range ps.Tracks {
				if t.Kind == "audio" {
					stat.AudioStreams++
				} else {
					stat.VideoStreams++
				}
			}
			stat.BitrateIn += ps.BitrateIn
			stat.BitrateOut += ps.BitrateOut
			snap.Subscriptions += ps.SubscribedCount
			snap.ParticipantStats = append(snap.ParticipantStats, ps)
		}
		snap.Participants += stat.Participants
		snap.AudioStreams += stat.AudioStreams
		snap.VideoStreams += stat.VideoStreams
		snap.BitrateIn += stat.BitrateIn
		snap.BitrateOut += stat.BitrateOut
		snap.Rooms = append(snap.Rooms, stat)
	}

	for _, snap := range projects {
		var rtt, jitter, loss float64
		var measured int
		for _, p := range snap.ParticipantStats {
			if p.RTTMs > 0 {
				rtt += p.RTTMs
				jitter += p.JitterMs
				measured++
			}
			loss = max(loss, p.LossRatio)
		}
		if measured > 0 {
			snap.AvgRTTMs = rtt / float64(measured)
			snap.AvgJitterMs = jitter / float64(measured)
		}
		snap.PacketLossRatio = loss
		// Each participant holds two peer connections.
		snap.Connections = snap.Participants * 2
	}
	for id, snap := range projects {
		prev := m.projects[id]
		cur := m.lifetime[id]
		if prev != nil && cur != nil {
			snap.PacketsInRate = int64(float64(cur.packetsIn-prev.totalPacketsIn) / seconds)
			snap.PacketsOutRate = int64(float64(cur.packetsOut-prev.totalPacketsOut) / seconds)
		}
		if cur != nil {
			snap.totalPacketsIn, snap.totalPacketsOut = cur.packetsIn, cur.packetsOut
		}
	}
	// Forget participants that have gone, so these maps do not grow forever on a long-lived node.
	for _, totals := range m.lifetime {
		for sid := range totals.seen {
			if _, live := next[sid]; !live {
				delete(totals.seen, sid)
				delete(totals.seenLost, sid)
			}
		}
	}
	m.projects = projects
	m.previous = next
	m.node = node
}

func (m *Monitor) participantStat(p *participant.Participant, roomName string, now time.Time, seconds float64, next map[string]counters, totals *lifetimeTotals) ParticipantStat {
	info := p.ToProto()
	s := p.Stats()
	ps := ParticipantStat{
		SID:             p.SID(),
		Identity:        p.Identity(),
		Name:            info.GetName(),
		Room:            roomName,
		State:           info.GetState().String(),
		Kind:            info.GetKind().String(),
		JoinedAt:        info.GetJoinedAt(),
		DurationSeconds: now.Unix() - info.GetJoinedAt(),
		SubscribedCount: len(p.SubscribedTrackSIDs()),
		BandwidthEst:    s.BandwidthEstimate,
		RTTMs:           float64(s.SubscribeRTT) / float64(time.Millisecond),
		JitterMs:        float64(s.SubscribeJitter) / float64(time.Millisecond),
		LossRatio:       float64(s.SubscribeLoss),
		PacketsOut:      s.SubscribedPackets,
		Permission:      info.GetPermission(),
		Tracks:          []TrackStat{},
	}
	score, quality := p.ConnectionQuality()
	ps.QualityScore, ps.Quality = score, quality

	var bytesIn, packetsIn, lostTotal uint64
	for _, t := range p.Tracks() {
		ti := t.Info()
		stat := TrackStat{
			SID:         t.SID(),
			Kind:        kindName(ti.GetType()),
			Source:      ti.GetSource().String(),
			MimeType:    ti.GetMimeType(),
			Muted:       ti.GetMuted(),
			Simulcast:   ti.GetSimulcast(),
			Width:       ti.GetWidth(),
			Height:      ti.GetHeight(),
			Subscribers: t.SubscriberCount(),
		}
		if ti.GetType() == pb.TrackType_AUDIO {
			ps.HasAudio = true
			ps.AudioMuted = ti.GetMuted()
		} else {
			ps.HasVideo = true
			ps.VideoMuted = ti.GetMuted()
		}
		if r := t.Receiver(); r != nil {
			rs := r.Stats()
			stat.BitrateIn = rs.Bitrate
			stat.FPS = rs.FPS
			stat.Layers = rs.Layers
			stat.PacketsIn = rs.Packets
			stat.PacketsLost = rs.Lost
			if ti.GetType() == pb.TrackType_VIDEO {
				if w, h, ok := topLayerSize(ti, rs); ok {
					stat.Width, stat.Height = w, h
				}
			}
			ps.BitrateIn += rs.Bitrate
			bytesIn += rs.Bytes
			packetsIn += rs.Packets
			lostTotal += rs.Lost
		}
		ps.Tracks = append(ps.Tracks, stat)
	}
	ps.PacketsIn = packetsIn

	// Rates come from the difference between samples, and a track ending can make a total go down.
	cur := counters{bytesIn: bytesIn, bytesOut: s.SubscribedBytes, packetsIn: packetsIn, packetsOut: s.SubscribedPackets}
	if prev, ok := m.previous[p.SID()]; ok {
		if cur.bytesOut >= prev.bytesOut {
			ps.BitrateOut = int64(float64(cur.bytesOut-prev.bytesOut) * 8 / seconds)
		}
	}
	next[p.SID()] = cur

	seen := totals.seen[p.SID()]
	totals.bytesIn += positiveDelta(cur.bytesIn, seen.bytesIn)
	totals.bytesOut += positiveDelta(cur.bytesOut, seen.bytesOut)
	totals.packetsIn += positiveDelta(cur.packetsIn, seen.packetsIn)
	totals.packetsOut += positiveDelta(cur.packetsOut, seen.packetsOut)
	totals.lost += positiveDelta(lostTotal, totals.seenLost[p.SID()])
	totals.seen[p.SID()] = cur
	totals.seenLost[p.SID()] = lostTotal
	return ps
}

func positiveDelta(cur, prev uint64) uint64 {
	if cur > prev {
		return cur - prev
	}
	return 0
}

func kindName(t pb.TrackType) string {
	if t == pb.TrackType_AUDIO {
		return "audio"
	}
	return "video"
}

// topLayerSize finds the resolution of the highest layer that is really sending.
func topLayerSize(ti *pb.TrackInfo, rs sfu.ReceiverStats) (uint32, uint32, bool) {
	var best *pb.VideoLayer
	for _, l := range ti.GetLayers() {
		q := int(l.GetQuality())
		if q < len(rs.LayerRates) && rs.LayerRates[q] > 0 && (best == nil || l.GetQuality() > best.GetQuality()) {
			best = l
		}
	}
	if best == nil {
		return 0, 0, false
	}
	return best.GetWidth(), best.GetHeight(), true
}

func (m *Monitor) nodeStats() NodeStats {
	n := NodeStats{
		NodeID:        m.rooms.NodeID(),
		Hostname:      m.host,
		NumCPU:        runtime.NumCPU(),
		Goroutines:    runtime.NumGoroutine(),
		UptimeSeconds: int64(time.Since(m.started).Seconds()),
		Draining:      m.rooms.Draining(),
		Healthy:       true,
	}
	if v, err := cpu.Percent(0, false); err == nil && len(v) > 0 {
		n.CPUPercent = v[0]
	}
	if v, err := mem.VirtualMemory(); err == nil {
		n.MemoryUsed, n.MemoryTotal = v.Used, v.Total
	}
	if m.proc != nil {
		if v, err := m.proc.Percent(0); err == nil {
			n.ProcessCPU = v
		}
		if v, err := m.proc.MemoryInfo(); err == nil && v != nil {
			n.ProcessMemory = v.RSS
		}
	}
	return n
}
