// Package routing decides which node hosts each room, so that everyone in a room reaches the same media
// server wherever they connect. Any node can accept any request: if another node hosts the room, the
// request is proxied there. Media does not pass through the proxy. Clients send it directly to the hosting
// node, which advertises its own address in ICE candidates.
package routing

import (
	"context"
	"errors"
)

// Node is a server in the cluster.
type Node struct {
	ID           string  `json:"id"`
	URL          string  `json:"url"`
	Region       string  `json:"region"`
	Rooms        int     `json:"rooms"`
	Participants int     `json:"participants"`
	CPUPercent   float64 `json:"cpu_percent"`
	Draining     bool    `json:"draining"`
}

// ErrNoNodes means no node can take a new room.
var ErrNoNodes = errors.New("no available servers")

// Router finds the node for a room.
type Router interface {
	// Route returns the node hosting a room. With create, a room that is not hosted anywhere is assigned to the
	// least busy node. Without it, ok is false for such a room.
	Route(ctx context.Context, project, room string, create bool) (node Node, ok bool, err error)
	// Release removes this node's claim on a room when it closes.
	Release(ctx context.Context, project, room string) error
	// Refresh keeps this node's claims and registration alive. It returns any of these rooms another live node has
	// claimed, which it leaves with that node: a copy here is one too many.
	Refresh(ctx context.Context, self Node, rooms []RoomRef) (lost []RoomRef, err error)
	// Nodes lists live nodes.
	Nodes(ctx context.Context) ([]Node, error)
	// SelfID returns this node's ID.
	SelfID() string
	// IsLocal reports whether this router has only one node, so nothing ever needs proxying.
	IsLocal() bool
}

// RoomRef names a room.
type RoomRef struct {
	Project string
	Name    string
}

// Local is the Router for a single server.
type Local struct{ ID string }

// Route implements Router.
func (l Local) Route(context.Context, string, string, bool) (Node, bool, error) {
	return Node{ID: l.ID}, true, nil
}

// Release implements Router.
func (Local) Release(context.Context, string, string) error { return nil }

// Refresh implements Router.
func (Local) Refresh(context.Context, Node, []RoomRef) ([]RoomRef, error) { return nil, nil }

// Nodes implements Router.
func (l Local) Nodes(context.Context) ([]Node, error) { return []Node{{ID: l.ID}}, nil }

// SelfID implements Router.
func (l Local) SelfID() string { return l.ID }

// IsLocal implements Router.
func (Local) IsLocal() bool { return true }
