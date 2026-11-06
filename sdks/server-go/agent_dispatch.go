package streamkit

import (
	"context"

	"elnova-streamkit.local/pkg/pb"
)

const agentDispatchService = "AgentDispatchService"

// AgentDispatchClient asks for agents to be sent to rooms, over the same Twirp service the reference server has. This
// server does not have that service yet, so each call returns an error wrapping ErrNotSupported here.
type AgentDispatchClient struct{ *base }

// NewAgentDispatchClient creates a client.
func NewAgentDispatchClient(serverURL, apiKey, apiSecret string, opts ...Option) *AgentDispatchClient {
	return &AgentDispatchClient{newBase(serverURL, apiKey, apiSecret, opts)}
}

// CreateDispatch sends an agent to a room.
func (c *AgentDispatchClient) CreateDispatch(ctx context.Context, in *pb.CreateAgentDispatchRequest) (*pb.AgentDispatch, error) {
	out := &pb.AgentDispatch{}
	return out, c.twirp(ctx, agentDispatchService, "CreateDispatch", admin(in.GetRoom()), in, out)
}

// DeleteDispatch cancels a dispatch.
func (c *AgentDispatchClient) DeleteDispatch(ctx context.Context, in *pb.DeleteAgentDispatchRequest) (*pb.AgentDispatch, error) {
	out := &pb.AgentDispatch{}
	return out, c.twirp(ctx, agentDispatchService, "DeleteDispatch", admin(in.GetRoom()), in, out)
}

// ListDispatch lists a room's dispatches.
func (c *AgentDispatchClient) ListDispatch(ctx context.Context, in *pb.ListAgentDispatchRequest) (*pb.ListAgentDispatchResponse, error) {
	out := &pb.ListAgentDispatchResponse{}
	return out, c.twirp(ctx, agentDispatchService, "ListDispatch", admin(in.GetRoom()), in, out)
}
