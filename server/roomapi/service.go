// Package roomapi serves the room management API over Twirp: POST requests to
// /twirp/livekit.RoomService/<Method>, with JSON or protobuf bodies. Server SDKs for this protocol call
// these paths, so their paths, encodings, error shapes and permission rules are all part of the contract.
package roomapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
)

const (
	pathPrefix = "/twirp/livekit.RoomService/"

	maxRequestBytes = 1 << 20
	maxNameLength   = 256
	nonceLength     = 16
)

// Service is the room API.
type Service struct {
	cfg   *config.Config
	keys  auth.KeyProvider
	rooms *room.Manager
	// RemoteRooms lists rooms on other nodes, on a cluster.
	RemoteRooms func(project string) []*pb.Room
	// Agents answers the agent dispatch API. Without one, that API reports itself as unimplemented.
	Agents AgentDispatcher
}

// NewService creates a Service.
func NewService(cfg *config.Config, keys auth.KeyProvider, rooms *room.Manager) *Service {
	return &Service{cfg: cfg, keys: keys, rooms: rooms}
}

// Register adds the API's routes to mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc(pathPrefix, s.serve)
	mux.HandleFunc(dispatchPathPrefix, s.serveDispatch)
}

// apiError is a failure with its Twirp code. The HTTP status follows from the code.
type apiError struct {
	code string
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func newError(code, format string, args ...any) *apiError {
	return &apiError{code, fmt.Sprintf(format, args...)}
}

var errDenied = newError("unauthenticated", "permissions denied")

var statusByCode = map[string]int{
	"canceled": 408, "unknown": 500, "invalid_argument": 400, "malformed": 400, "deadline_exceeded": 408,
	"not_found": 404, "bad_route": 404, "already_exists": 409, "permission_denied": 403, "resource_exhausted": 429,
	"failed_precondition": 412, "aborted": 409, "out_of_range": 400, "unimplemented": 501, "internal": 500,
	"unavailable": 503, "data_loss": 500, "unauthenticated": 401,
}

func writeError(w http.ResponseWriter, e *apiError) {
	status, ok := statusByCode[e.code]
	if !ok {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Go Twirp clients reject error bodies with unknown keys, so this has exactly these two.
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(e.msg)
	fmt.Fprintf(w, `{"code":%q,"msg":"%s"}`, e.code, escaped)
}

// request carries what every method needs.
type request struct {
	project string
	grants  *auth.Grants
}

func (r *request) needCreate() *apiError {
	if !r.grants.Video.RoomCreate {
		return errDenied
	}
	return nil
}

func (r *request) needList() *apiError {
	if !r.grants.Video.RoomList {
		return errDenied
	}
	return nil
}

// needAdmin requires the admin grant for this exact room. There is no wildcard.
func (r *request) needAdmin(roomName string) *apiError {
	if !r.grants.Video.RoomAdmin || r.grants.Video.Room != roomName {
		return errDenied
	}
	return nil
}

func method[Req, Res proto.Message](newReq func() Req, fn func(*request, Req) (Res, *apiError)) func(*request, http.ResponseWriter, []byte, bool) {
	return func(r *request, w http.ResponseWriter, body []byte, isJSON bool) {
		req := newReq()
		if isJSON {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, req); err != nil {
				writeError(w, newError("malformed", "the json request could not be decoded"))
				return
			}
		} else if err := proto.Unmarshal(body, req); err != nil {
			writeError(w, newError("malformed", "the protobuf request could not be decoded"))
			return
		}
		res, apiErr := fn(r, req)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
		var data []byte
		var err error
		if isJSON {
			// Responses use proto field names and include default values.
			data, err = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(res)
			w.Header().Set("Content-Type", "application/json")
		} else {
			data, err = proto.Marshal(res)
			w.Header().Set("Content-Type", "application/protobuf")
		}
		if err != nil {
			writeError(w, newError("internal", "could not encode the response"))
			return
		}
		_, _ = w.Write(data)
	}
}

func (s *Service) handlers() map[string]func(*request, http.ResponseWriter, []byte, bool) {
	return map[string]func(*request, http.ResponseWriter, []byte, bool){
		"CreateRoom":          method(func() *pb.CreateRoomRequest { return &pb.CreateRoomRequest{} }, s.createRoom),
		"ListRooms":           method(func() *pb.ListRoomsRequest { return &pb.ListRoomsRequest{} }, s.listRooms),
		"DeleteRoom":          method(func() *pb.DeleteRoomRequest { return &pb.DeleteRoomRequest{} }, s.deleteRoom),
		"ListParticipants":    method(func() *pb.ListParticipantsRequest { return &pb.ListParticipantsRequest{} }, s.listParticipants),
		"GetParticipant":      method(func() *pb.RoomParticipantIdentity { return &pb.RoomParticipantIdentity{} }, s.getParticipant),
		"RemoveParticipant":   method(func() *pb.RoomParticipantIdentity { return &pb.RoomParticipantIdentity{} }, s.removeParticipant),
		"MutePublishedTrack":  method(func() *pb.MuteRoomTrackRequest { return &pb.MuteRoomTrackRequest{} }, s.muteTrack),
		"UpdateParticipant":   method(func() *pb.UpdateParticipantRequest { return &pb.UpdateParticipantRequest{} }, s.updateParticipant),
		"UpdateSubscriptions": method(func() *pb.UpdateSubscriptionsRequest { return &pb.UpdateSubscriptionsRequest{} }, s.updateSubscriptions),
		"SendData":            method(func() *pb.SendDataRequest { return &pb.SendDataRequest{} }, s.sendData),
		"UpdateRoomMetadata":  method(func() *pb.UpdateRoomMetadataRequest { return &pb.UpdateRoomMetadataRequest{} }, s.updateRoomMetadata),
		"ForwardParticipant":  method(func() *pb.ForwardParticipantRequest { return &pb.ForwardParticipantRequest{} }, unimplemented[*pb.ForwardParticipantRequest, *pb.ForwardParticipantResponse]),
		"MoveParticipant":     method(func() *pb.MoveParticipantRequest { return &pb.MoveParticipantRequest{} }, unimplemented[*pb.MoveParticipantRequest, *pb.MoveParticipantResponse]),
		"PerformRpc":          method(func() *pb.PerformRpcRequest { return &pb.PerformRpcRequest{} }, unimplemented[*pb.PerformRpcRequest, *pb.PerformRpcResponse]),
	}
}

func unimplemented[Req, Res proto.Message](*request, Req) (Res, *apiError) {
	var zero Res
	return zero, newError("unimplemented", "not implemented")
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	s.serveMethods(w, r, pathPrefix, s.handlers())
}

func (s *Service) serveMethods(w http.ResponseWriter, r *http.Request, prefix string, handlers map[string]func(*request, http.ResponseWriter, []byte, bool)) {
	path := r.URL.Path
	if r.Method != http.MethodPost {
		writeError(w, newError("bad_route", "unsupported method %q (only POST is allowed)", r.Method))
		return
	}
	handler, ok := handlers[strings.TrimPrefix(path, prefix)]
	if !ok {
		writeError(w, newError("bad_route", "no handler for path %q", path))
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && contentType != "application/protobuf") {
		writeError(w, newError("bad_route", "unexpected Content-Type: %q", r.Header.Get("Content-Type")))
		return
	}

	req := &request{grants: &auth.Grants{Video: &auth.VideoGrant{}}}
	if header := r.Header.Get("Authorization"); header != "" {
		token := auth.TokenFromHeader(header)
		if token == "" {
			// Authentication failures are plain text, outside the Twirp error shape.
			http.Error(w, "invalid authorization header. Must start with Bearer ", http.StatusUnauthorized)
			return
		}
		verified, err := auth.Verify(token, s.keys)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		req.grants = verified.Grants
		req.project = verified.Key.ProjectID
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, newError("malformed", "the request could not be read"))
		return
	}
	handler(req, w, body, contentType == "application/json")
}

func (s *Service) createRoom(r *request, in *pb.CreateRoomRequest) (*pb.Room, *apiError) {
	if err := r.needCreate(); err != nil {
		return nil, err
	}
	if in.GetName() == "" {
		return nil, newError("invalid_argument", "no room name")
	}
	if len(in.GetName()) > maxNameLength {
		return nil, newError("invalid_argument", "room name length exceeds limits: max length %d", maxNameLength)
	}
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(in.GetMetadata()) > limit {
		return nil, newError("invalid_argument", "metadata size exceeds limits %d", limit)
	}
	// Creating a room that exists returns it unchanged.
	created, _ := s.rooms.GetOrCreate(r.project, in.GetName(), room.CreateOptions{
		EmptyTimeout:     in.GetEmptyTimeout(),
		DepartureTimeout: in.GetDepartureTimeout(),
		MaxParticipants:  in.GetMaxParticipants(),
		Metadata:         in.GetMetadata(),
		Agents:           in.GetAgents(),
		SyncStreams:      in.GetSyncStreams(),
	})
	return created.ToProto(), nil
}

func (s *Service) listRooms(r *request, in *pb.ListRoomsRequest) (*pb.ListRoomsResponse, *apiError) {
	if err := r.needList(); err != nil {
		return nil, err
	}
	rooms := s.rooms.List(r.project, in.GetNames())
	out := &pb.ListRoomsResponse{Rooms: make([]*pb.Room, 0, len(rooms))}
	for _, x := range rooms {
		out.Rooms = append(out.Rooms, x.ToProto())
	}
	if s.RemoteRooms != nil {
		for _, x := range s.RemoteRooms(r.project) {
			if len(in.GetNames()) == 0 || slices.Contains(in.GetNames(), x.GetName()) {
				out.Rooms = append(out.Rooms, x)
			}
		}
	}
	slices.SortFunc(out.Rooms, func(a, b *pb.Room) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, nil
}

func (s *Service) deleteRoom(r *request, in *pb.DeleteRoomRequest) (*pb.DeleteRoomResponse, *apiError) {
	if err := r.needCreate(); err != nil {
		return nil, err
	}
	if err := s.rooms.Delete(r.project, in.GetRoom()); err != nil {
		return nil, newError("not_found", "%s", err)
	}
	return &pb.DeleteRoomResponse{}, nil
}

func (s *Service) listParticipants(r *request, in *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	out := &pb.ListParticipantsResponse{Participants: []*pb.ParticipantInfo{}}
	// An unknown room has no participants, which is not an error.
	if x := s.rooms.Get(r.project, in.GetRoom()); x != nil {
		for _, p := range x.Participants() {
			out.Participants = append(out.Participants, p.ToProto())
		}
	}
	return out, nil
}

// find locates a participant, reporting the errors API clients expect.
func (s *Service) find(r *request, roomName, identity string) (*room.Room, *participant.Participant, *apiError) {
	if err := r.needAdmin(roomName); err != nil {
		return nil, nil, err
	}
	x := s.rooms.Get(r.project, roomName)
	if x == nil {
		return nil, nil, newError("not_found", "%s", room.ErrRoomNotFound)
	}
	p := x.Participant(identity)
	if p == nil {
		return nil, nil, newError("not_found", "%s", room.ErrParticipantNotFound)
	}
	return x, p, nil
}

func (s *Service) getParticipant(r *request, in *pb.RoomParticipantIdentity) (*pb.ParticipantInfo, *apiError) {
	_, p, err := s.find(r, in.GetRoom(), in.GetIdentity())
	if err != nil {
		return nil, err
	}
	return p.ToProto(), nil
}

// removeParticipant answers as the reference does for someone who is not connected. If a share link let them in, they are
// still refused from now on.
func (s *Service) removeParticipant(r *request, in *pb.RoomParticipantIdentity) (*pb.RemoveParticipantResponse, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	if !s.rooms.Remove(r.project, in.GetRoom(), in.GetIdentity()) {
		if s.rooms.Get(r.project, in.GetRoom()) == nil {
			return nil, newError("not_found", "%s", room.ErrRoomNotFound)
		}
		return nil, newError("not_found", "%s", room.ErrParticipantNotFound)
	}
	return &pb.RemoveParticipantResponse{}, nil
}

func (s *Service) muteTrack(r *request, in *pb.MuteRoomTrackRequest) (*pb.MuteRoomTrackResponse, *apiError) {
	if !in.GetMuted() && !s.cfg.Room.EnableRemoteUnmute {
		if err := r.needAdmin(in.GetRoom()); err != nil {
			return nil, err
		}
		return nil, newError("failed_precondition", "remote unmute not enabled")
	}
	_, p, err := s.find(r, in.GetRoom(), in.GetIdentity())
	if err != nil {
		return nil, err
	}
	info, mErr := p.SetTrackMuted(in.GetTrackSid(), in.GetMuted())
	if errors.Is(mErr, participant.ErrTrackNotFound) {
		return nil, newError("not_found", "%s", room.ErrTrackNotFound)
	}
	if mErr != nil {
		return nil, newError("internal", "%s", mErr)
	}
	return &pb.MuteRoomTrackResponse{Track: info}, nil
}

func (s *Service) updateParticipant(r *request, in *pb.UpdateParticipantRequest) (*pb.ParticipantInfo, *apiError) {
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(in.GetMetadata()) > limit {
		return nil, newError("invalid_argument", "metadata size exceeds limits %d", limit)
	}
	if len(in.GetName()) > maxNameLength {
		return nil, newError("invalid_argument", "name length exceeds limits %d", maxNameLength)
	}
	_, p, err := s.find(r, in.GetRoom(), in.GetIdentity())
	if err != nil {
		return nil, err
	}
	// An empty name or metadata means unchanged. A permission replaces the whole set.
	p.UpdateInfo(&in.Metadata, &in.Name, in.GetAttributes(), in.GetPermission())
	return p.ToProto(), nil
}

func (s *Service) updateSubscriptions(r *request, in *pb.UpdateSubscriptionsRequest) (*pb.UpdateSubscriptionsResponse, *apiError) {
	x, p, err := s.find(r, in.GetRoom(), in.GetIdentity())
	if err != nil {
		return nil, err
	}
	sids := slices.Clone(in.GetTrackSids())
	for _, pt := range in.GetParticipantTracks() {
		sids = append(sids, pt.GetTrackSids()...)
	}
	x.UpdateSubscriptions(p, sids, in.GetSubscribe())
	return &pb.UpdateSubscriptionsResponse{}, nil
}

func (s *Service) sendData(r *request, in *pb.SendDataRequest) (*pb.SendDataResponse, *apiError) {
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	if n := len(in.GetNonce()); n != 0 && n != nonceLength {
		return nil, newError("invalid_argument", "nonce should be 16-bytes or not present, got: %d bytes", n)
	}
	x := s.rooms.Get(r.project, in.GetRoom())
	if x == nil {
		return nil, newError("not_found", "%s", room.ErrRoomNotFound)
	}
	x.SendData(in.GetKind(), &pb.DataPacket{
		DestinationIdentities: in.GetDestinationIdentities(),
		Value: &pb.DataPacket_User{User: &pb.UserPacket{
			Payload:               in.GetData(),
			Topic:                 in.Topic,
			DestinationIdentities: in.GetDestinationIdentities(),
			Nonce:                 in.GetNonce(),
		}},
	})
	return &pb.SendDataResponse{}, nil
}

func (s *Service) updateRoomMetadata(r *request, in *pb.UpdateRoomMetadataRequest) (*pb.Room, *apiError) {
	if limit := s.cfg.Limits.MaxMetadataBytes; limit > 0 && len(in.GetMetadata()) > limit {
		return nil, newError("invalid_argument", "metadata size exceeds limits %d", limit)
	}
	if err := r.needAdmin(in.GetRoom()); err != nil {
		return nil, err
	}
	x := s.rooms.Get(r.project, in.GetRoom())
	if x == nil {
		return nil, newError("not_found", "%s", room.ErrRoomNotFound)
	}
	// Unlike participant metadata, an empty value here does clear it.
	return x.UpdateMetadata(in.GetMetadata()), nil
}
