package roomapi

import (
	"errors"
	"net/http"

	"elnova-streamkit.local/pkg/pb"
)

const dispatchPathPrefix = "/twirp/livekit.AgentDispatchService/"

// Errors an AgentDispatcher reports, which become Twirp codes.
var (
	ErrDispatchNotFound = errors.New("dispatch ID not found")
	ErrRoomNotFound     = errors.New("requested room does not exist")
)

// AgentDispatcher is the server's job broker, as the dispatch API sees it.
type AgentDispatcher interface {
	CreateDispatch(project string, in *pb.CreateAgentDispatchRequest) (*pb.AgentDispatch, error)
	DeleteDispatch(project string, in *pb.DeleteAgentDispatchRequest) (*pb.AgentDispatch, error)
	ListDispatch(project string, in *pb.ListAgentDispatchRequest) ([]*pb.AgentDispatch, error)
}

func (s *Service) serveDispatch(w http.ResponseWriter, r *http.Request) {
	s.serveMethods(w, r, dispatchPathPrefix, map[string]func(*request, http.ResponseWriter, []byte, bool){
		"CreateDispatch": method(func() *pb.CreateAgentDispatchRequest { return &pb.CreateAgentDispatchRequest{} }, s.createDispatch),
		"DeleteDispatch": method(func() *pb.DeleteAgentDispatchRequest { return &pb.DeleteAgentDispatchRequest{} }, s.deleteDispatch),
		"ListDispatch":   method(func() *pb.ListAgentDispatchRequest { return &pb.ListAgentDispatchRequest{} }, s.listDispatch),
	})
}

func dispatchError(err error) *apiError {
	switch {
	case errors.Is(err, ErrDispatchNotFound), errors.Is(err, ErrRoomNotFound):
		return newError("not_found", "%s", err.Error())
	}
	return newError("invalid_argument", "%s", err.Error())
}

func (s *Service) createDispatch(r *request, in *pb.CreateAgentDispatchRequest) (*pb.AgentDispatch, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	if s.Agents == nil {
		return nil, newError("unimplemented", "agents are not enabled")
	}
	if len(in.GetRoom()) > maxNameLength {
		return nil, newError("invalid_argument", "room name length exceeds limits %d", maxNameLength)
	}
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(in.GetMetadata()) > limit {
		return nil, newError("invalid_argument", "metadata size exceeds limits %d", limit)
	}
	d, err := s.Agents.CreateDispatch(r.project, in)
	if err != nil {
		return nil, dispatchError(err)
	}
	return d, nil
}

func (s *Service) deleteDispatch(r *request, in *pb.DeleteAgentDispatchRequest) (*pb.AgentDispatch, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	if s.Agents == nil {
		return nil, newError("unimplemented", "agents are not enabled")
	}
	d, err := s.Agents.DeleteDispatch(r.project, in)
	if err != nil {
		return nil, dispatchError(err)
	}
	return d, nil
}

func (s *Service) listDispatch(r *request, in *pb.ListAgentDispatchRequest) (*pb.ListAgentDispatchResponse, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	if s.Agents == nil {
		return nil, newError("unimplemented", "agents are not enabled")
	}
	list, err := s.Agents.ListDispatch(r.project, in)
	if err != nil {
		return nil, dispatchError(err)
	}
	return &pb.ListAgentDispatchResponse{AgentDispatches: list}, nil
}
