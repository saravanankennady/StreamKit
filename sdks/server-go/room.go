package streamkit

import (
	"context"
	"crypto/rand"

	"elnova-streamkit.local/pkg/pb"
)

const roomService = "RoomService"

// RoomClient calls the room API. Each call signs its own short-lived token, carrying only the grant that call needs.
type RoomClient struct{ *base }

// NewRoomClient creates a client. serverURL may be the ws:// address clients connect to.
func NewRoomClient(serverURL, apiKey, apiSecret string, opts ...Option) *RoomClient {
	return &RoomClient{newBase(serverURL, apiKey, apiSecret, opts)}
}

func admin(room string) *VideoGrant { return &VideoGrant{RoomAdmin: true, Room: room} }

// CreateToken starts a token signed with this client's key and secret.
func (c *RoomClient) CreateToken() *AccessToken { return NewAccessToken(c.apiKey, c.apiSecret) }

// CreateRoom creates a room, or returns it unchanged if it is already open.
func (c *RoomClient) CreateRoom(ctx context.Context, in *pb.CreateRoomRequest) (*pb.Room, error) {
	out := &pb.Room{}
	return out, c.twirp(ctx, roomService, "CreateRoom", &VideoGrant{RoomCreate: true}, in, out)
}

// ListRooms lists open rooms, optionally only those named.
func (c *RoomClient) ListRooms(ctx context.Context, in *pb.ListRoomsRequest) (*pb.ListRoomsResponse, error) {
	out := &pb.ListRoomsResponse{}
	return out, c.twirp(ctx, roomService, "ListRooms", &VideoGrant{RoomList: true}, in, out)
}

// DeleteRoom closes a room and disconnects everyone in it.
func (c *RoomClient) DeleteRoom(ctx context.Context, in *pb.DeleteRoomRequest) (*pb.DeleteRoomResponse, error) {
	out := &pb.DeleteRoomResponse{}
	return out, c.twirp(ctx, roomService, "DeleteRoom", &VideoGrant{RoomCreate: true}, in, out)
}

// UpdateRoomMetadata replaces a room's metadata. An empty value clears it.
func (c *RoomClient) UpdateRoomMetadata(ctx context.Context, in *pb.UpdateRoomMetadataRequest) (*pb.Room, error) {
	out := &pb.Room{}
	return out, c.twirp(ctx, roomService, "UpdateRoomMetadata", admin(in.GetRoom()), in, out)
}

// ListParticipants lists a room's participants. An unknown room has none, and that is not an error.
func (c *RoomClient) ListParticipants(ctx context.Context, in *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error) {
	out := &pb.ListParticipantsResponse{}
	return out, c.twirp(ctx, roomService, "ListParticipants", admin(in.GetRoom()), in, out)
}

// GetParticipant returns one participant.
func (c *RoomClient) GetParticipant(ctx context.Context, in *pb.RoomParticipantIdentity) (*pb.ParticipantInfo, error) {
	out := &pb.ParticipantInfo{}
	return out, c.twirp(ctx, roomService, "GetParticipant", admin(in.GetRoom()), in, out)
}

// RemoveParticipant disconnects a participant.
func (c *RoomClient) RemoveParticipant(ctx context.Context, in *pb.RoomParticipantIdentity) (*pb.RemoveParticipantResponse, error) {
	out := &pb.RemoveParticipantResponse{}
	return out, c.twirp(ctx, roomService, "RemoveParticipant", admin(in.GetRoom()), in, out)
}

// MutePublishedTrack mutes a participant's track. Unmuting needs room.enable_remote_unmute on the server.
func (c *RoomClient) MutePublishedTrack(ctx context.Context, in *pb.MuteRoomTrackRequest) (*pb.MuteRoomTrackResponse, error) {
	out := &pb.MuteRoomTrackResponse{}
	return out, c.twirp(ctx, roomService, "MutePublishedTrack", admin(in.GetRoom()), in, out)
}

// UpdateParticipant changes a participant's metadata, name, attributes or permissions. Empty metadata and name mean
// unchanged. A permission replaces the whole set.
func (c *RoomClient) UpdateParticipant(ctx context.Context, in *pb.UpdateParticipantRequest) (*pb.ParticipantInfo, error) {
	out := &pb.ParticipantInfo{}
	return out, c.twirp(ctx, roomService, "UpdateParticipant", admin(in.GetRoom()), in, out)
}

// UpdateSubscriptions subscribes a participant to tracks, or unsubscribes them.
func (c *RoomClient) UpdateSubscriptions(ctx context.Context, in *pb.UpdateSubscriptionsRequest) (*pb.UpdateSubscriptionsResponse, error) {
	out := &pb.UpdateSubscriptionsResponse{}
	return out, c.twirp(ctx, roomService, "UpdateSubscriptions", admin(in.GetRoom()), in, out)
}

// SendData sends a data packet to a room, or to the identities named.
func (c *RoomClient) SendData(ctx context.Context, in *pb.SendDataRequest) (*pb.SendDataResponse, error) {
	if len(in.GetNonce()) == 0 {
		// Lets a client spot a packet it has already seen, when a call is retried.
		in.Nonce = make([]byte, 16)
		if _, err := rand.Read(in.Nonce); err != nil {
			return nil, err
		}
	}
	out := &pb.SendDataResponse{}
	return out, c.twirp(ctx, roomService, "SendData", admin(in.GetRoom()), in, out)
}

// ForwardParticipant forwards a participant's tracks to another room. This server does not implement it, and neither
// does the open-source reference server: the error wraps ErrNotSupported here.
func (c *RoomClient) ForwardParticipant(ctx context.Context, in *pb.ForwardParticipantRequest) (*pb.ForwardParticipantResponse, error) {
	out := &pb.ForwardParticipantResponse{}
	grant := &VideoGrant{RoomAdmin: true, Room: in.GetRoom(), DestinationRoom: in.GetDestinationRoom()}
	return out, c.twirp(ctx, roomService, "ForwardParticipant", grant, in, out)
}

// MoveParticipant moves a participant to another room. This server does not implement it, and neither does the
// open-source reference server: the error wraps ErrNotSupported here.
func (c *RoomClient) MoveParticipant(ctx context.Context, in *pb.MoveParticipantRequest) (*pb.MoveParticipantResponse, error) {
	out := &pb.MoveParticipantResponse{}
	grant := &VideoGrant{RoomAdmin: true, Room: in.GetRoom(), DestinationRoom: in.GetDestinationRoom()}
	return out, c.twirp(ctx, roomService, "MoveParticipant", grant, in, out)
}

// PerformRpc asks a participant to run a method. This server does not implement it yet: the error wraps ErrNotSupported.
func (c *RoomClient) PerformRpc(ctx context.Context, in *pb.PerformRpcRequest) (*pb.PerformRpcResponse, error) {
	out := &pb.PerformRpcResponse{}
	return out, c.twirp(ctx, roomService, "PerformRpc", admin(in.GetRoom()), in, out)
}
