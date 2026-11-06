package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// How long claims and registrations last without a refresh. A crashed node's rooms become claimable after
// nodeTTL, because liveness is checked before trusting a claim. The refresh interval must be well below both.
const (
	nodeTTL = 10 * time.Second
	roomTTL = 30 * time.Second
	// RefreshInterval is how often Refresh should run.
	RefreshInterval = 3 * time.Second

	nodesKey      = "streamkit:nodes"
	nodeKeyPrefix = "streamkit:node:"
)

func nodeKey(id string) string { return nodeKeyPrefix + id }

func roomKey(project, room string) string {
	// Length-prefixing keeps project and room separate whatever characters they contain.
	return fmt.Sprintf("streamkit:room:%d:%s:%s", len(project), project, room)
}

// Compare-and-swap in Redis, so two nodes reclaiming a dead node's room cannot both win.
var (
	casScript = redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
			return 1
		end
		return 0`)
	deleteIfOwnerScript = redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('DEL', KEYS[1])
		end
		return 0`)
	// A node keeps a room it holds if the claim is its own, nobody's, or that of a node that has gone. Another live node's
	// claim stands.
	keepRoomScript = redis.NewScript(`
		local owner = redis.call('GET', KEYS[1])
		if owner == false or owner == ARGV[1] or redis.call('EXISTS', ARGV[3] .. owner) == 0 then
			redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
			return 1
		end
		return 0`)
)

// Redis is the Router for a cluster.
type Redis struct {
	client *redis.Client
	selfID string
}

// NewRedis connects to Redis. url looks like redis://host:port/db.
func NewRedis(ctx context.Context, url, selfID string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &Redis{client: client, selfID: selfID}, nil
}

// Ping checks the connection, for readiness probes.
func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

// Close releases the connection.
func (r *Redis) Close() error { return r.client.Close() }

// SelfID implements Router.
func (r *Redis) SelfID() string { return r.selfID }

// IsLocal implements Router.
func (*Redis) IsLocal() bool { return false }

// Refresh implements Router.
func (r *Redis) Refresh(ctx context.Context, self Node, rooms []RoomRef) ([]RoomRef, error) {
	data, err := json.Marshal(self)
	if err != nil {
		return nil, err
	}
	pipe := r.client.Pipeline()
	pipe.Set(ctx, nodeKey(self.ID), data, nodeTTL)
	pipe.SAdd(ctx, nodesKey, self.ID)
	kept := make([]*redis.Cmd, len(rooms))
	for i, room := range rooms {
		// This also restores a claim that was lost, say to a Redis restart.
		kept[i] = keepRoomScript.Eval(ctx, pipe, []string{roomKey(room.Project, room.Name)}, self.ID, roomTTL.Milliseconds(), nodeKeyPrefix)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	var lost []RoomRef
	for i, cmd := range kept {
		won, err := cmd.Bool()
		if err != nil {
			return nil, err
		}
		if !won {
			lost = append(lost, rooms[i])
		}
	}
	return lost, nil
}

// Nodes implements Router. It also tidies the set of nodes, which has no expiry of its own.
func (r *Redis) Nodes(ctx context.Context) ([]Node, error) {
	ids, err := r.client.SMembers(ctx, nodesKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		n, ok, err := r.node(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			r.client.SRem(ctx, nodesKey, id)
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (r *Redis) node(ctx context.Context, id string) (Node, bool, error) {
	data, err := r.client.Get(ctx, nodeKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, err
	}
	var n Node
	if err := json.Unmarshal(data, &n); err != nil {
		return Node{}, false, err
	}
	return n, true, nil
}

// Route implements Router.
func (r *Redis) Route(ctx context.Context, project, room string, create bool) (Node, bool, error) {
	key := roomKey(project, room)
	ownerID, err := r.client.Get(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Node{}, false, err
	}
	claimed := err == nil
	if claimed {
		owner, alive, err := r.node(ctx, ownerID)
		if err != nil {
			return Node{}, false, err
		}
		if alive {
			return owner, true, nil
		}
	}
	if !create {
		return Node{}, false, nil
	}

	nodes, err := r.Nodes(ctx)
	if err != nil {
		return Node{}, false, err
	}
	best, ok := leastLoaded(nodes)
	if !ok {
		return Node{}, false, ErrNoNodes
	}
	var won bool
	if claimed {
		// The owner is dead. Whoever swaps it first wins, and everyone else follows the winner.
		won, err = casScript.Run(ctx, r.client, []string{key}, ownerID, best.ID, roomTTL.Milliseconds()).Bool()
	} else {
		won, err = r.client.SetNX(ctx, key, best.ID, roomTTL).Result()
	}
	if err != nil {
		return Node{}, false, err
	}
	if won {
		return best, true, nil
	}
	// Someone else just claimed it. Follow their choice.
	winnerID, err := r.client.Get(ctx, key).Result()
	if err != nil {
		return Node{}, false, err
	}
	winner, alive, err := r.node(ctx, winnerID)
	if err != nil {
		return Node{}, false, err
	}
	if !alive {
		return Node{}, false, ErrNoNodes
	}
	return winner, true, nil
}

// leastLoaded picks the node with the most headroom. CPU decides, because media load varies enormously per
// participant, and participant count separates nodes that are about equal.
func leastLoaded(nodes []Node) (Node, bool) {
	candidates := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if !n.Draining {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return Node{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		// CPU readings are noisy, so small differences do not count.
		const cpuBand = 10
		if ca, cb := int(a.CPUPercent)/cpuBand, int(b.CPUPercent)/cpuBand; ca != cb {
			return ca < cb
		}
		if a.Participants != b.Participants {
			return a.Participants < b.Participants
		}
		if a.Rooms != b.Rooms {
			return a.Rooms < b.Rooms
		}
		return a.ID < b.ID
	})
	return candidates[0], true
}

// Release implements Router.
func (r *Redis) Release(ctx context.Context, project, room string) error {
	return deleteIfOwnerScript.Run(ctx, r.client, []string{roomKey(project, room)}, r.selfID).Err()
}

// PublishSnapshot shares a project's live stats with other nodes.
func (r *Redis) PublishSnapshot(ctx context.Context, project string, data []byte) error {
	return r.client.Set(ctx, snapshotKey(project, r.selfID), data, snapshotTTL).Err()
}

// Snapshots returns other nodes' current stats for a project.
func (r *Redis) Snapshots(ctx context.Context, project string, nodes []Node) ([][]byte, error) {
	keys := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ID != r.selfID {
			keys = append(keys, snapshotKey(project, n.ID))
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	values, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, []byte(s))
		}
	}
	return out, nil
}

const snapshotTTL = 5 * time.Second

func snapshotKey(project, node string) string { return "streamkit:snapshot:" + project + ":" + node }
