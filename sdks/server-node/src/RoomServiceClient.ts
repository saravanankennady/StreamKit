import { create, fromJson, toJson, type JsonValue, type MessageInitShape } from '@bufbuild/protobuf';
import type { VideoGrant } from './grants.js';
import { Transport, type ClientOptions } from './http.js';
import { DataPacket_Kind, ParticipantInfoSchema, ParticipantPermissionSchema, RoomSchema, TrackInfoSchema, type ParticipantInfo, type Room, type TrackInfo } from './proto/livekit_models_pb.js';
import {
  CreateRoomRequestSchema,
  DeleteRoomRequestSchema,
  ForwardParticipantRequestSchema,
  ListParticipantsRequestSchema,
  ListParticipantsResponseSchema,
  ListRoomsRequestSchema,
  ListRoomsResponseSchema,
  MoveParticipantRequestSchema,
  MuteRoomTrackRequestSchema,
  MuteRoomTrackResponseSchema,
  PerformRpcRequestSchema,
  PerformRpcResponseSchema,
  RoomParticipantIdentitySchema,
  SendDataRequestSchema,
  UpdateParticipantRequestSchema,
  UpdateRoomMetadataRequestSchema,
  UpdateSubscriptionsRequestSchema,
} from './proto/livekit_room_pb.js';

const SERVICE = 'RoomService';

export interface CreateOptions {
  /** The name of the room. */
  name: string;
  /** Seconds to keep the room open before anyone joins. */
  emptyTimeout?: number;
  /** Seconds to keep the room open after the last person leaves. */
  departureTimeout?: number;
  /** The most people allowed at once. */
  maxParticipants?: number;
  /** The room's metadata. */
  metadata?: string;
}

export interface UpdateParticipantOptions {
  /** Empty leaves it unchanged. */
  metadata?: string;
  /** Empty leaves it unchanged. */
  name?: string;
  /** Only the attributes named change. An empty value removes one. */
  attributes?: Record<string, string>;
  /** Replaces the whole set of permissions. */
  permission?: MessageInitShape<typeof ParticipantPermissionSchema>;
}

export interface SendDataOptions {
  /** Only these identities get it. By default everyone does. */
  destinationIdentities?: string[];
  topic?: string;
}

const parse = { ignoreUnknownFields: true };

function admin(room: string): VideoGrant {
  return { roomAdmin: true, room };
}

/** Calls the room API. Each call signs its own short-lived token, carrying only the grant that call needs. */
export class RoomServiceClient {
  private readonly transport: Transport;

  /** @param url - the server, which may be the ws:// address clients connect to */
  constructor(url: string, apiKey?: string, apiSecret?: string, options?: ClientOptions) {
    this.transport = new Transport(url, apiKey, apiSecret, options);
  }

  private call(method: string, grant: VideoGrant, body: JsonValue): Promise<JsonValue> {
    return this.transport.twirp(SERVICE, method, grant, body) as Promise<JsonValue>;
  }

  /** Creates a room, or returns it unchanged if it is already open. */
  async createRoom(options: CreateOptions): Promise<Room> {
    const body = toJson(CreateRoomRequestSchema, create(CreateRoomRequestSchema, options));
    return fromJson(RoomSchema, await this.call('CreateRoom', { roomCreate: true }, body), parse);
  }

  /** Lists open rooms, optionally only those named. */
  async listRooms(names?: string[]): Promise<Room[]> {
    const body = toJson(ListRoomsRequestSchema, create(ListRoomsRequestSchema, { names: names ?? [] }));
    return fromJson(ListRoomsResponseSchema, await this.call('ListRooms', { roomList: true }, body), parse).rooms;
  }

  /** Closes a room and disconnects everyone in it. */
  async deleteRoom(room: string): Promise<void> {
    const body = toJson(DeleteRoomRequestSchema, create(DeleteRoomRequestSchema, { room }));
    await this.call('DeleteRoom', { roomCreate: true }, body);
  }

  /** Replaces a room's metadata. An empty value clears it. */
  async updateRoomMetadata(room: string, metadata: string): Promise<Room> {
    const body = toJson(UpdateRoomMetadataRequestSchema, create(UpdateRoomMetadataRequestSchema, { room, metadata }));
    return fromJson(RoomSchema, await this.call('UpdateRoomMetadata', admin(room), body), parse);
  }

  /** Lists a room's participants. An unknown room has none, and that is not an error. */
  async listParticipants(room: string): Promise<ParticipantInfo[]> {
    const body = toJson(ListParticipantsRequestSchema, create(ListParticipantsRequestSchema, { room }));
    return fromJson(ListParticipantsResponseSchema, await this.call('ListParticipants', admin(room), body), parse).participants;
  }

  /** Returns one participant. */
  async getParticipant(room: string, identity: string): Promise<ParticipantInfo> {
    const body = toJson(RoomParticipantIdentitySchema, create(RoomParticipantIdentitySchema, { room, identity }));
    return fromJson(ParticipantInfoSchema, await this.call('GetParticipant', admin(room), body), parse);
  }

  /** Disconnects a participant. */
  async removeParticipant(room: string, identity: string): Promise<void> {
    const body = toJson(RoomParticipantIdentitySchema, create(RoomParticipantIdentitySchema, { room, identity }));
    await this.call('RemoveParticipant', admin(room), body);
  }

  /** Mutes a participant's track. Unmuting needs room.enable_remote_unmute on the server. */
  async mutePublishedTrack(room: string, identity: string, trackSid: string, muted: boolean): Promise<TrackInfo> {
    const body = toJson(MuteRoomTrackRequestSchema, create(MuteRoomTrackRequestSchema, { room, identity, trackSid, muted }));
    const res = fromJson(MuteRoomTrackResponseSchema, await this.call('MutePublishedTrack', admin(room), body), parse);
    return res.track ?? create(TrackInfoSchema);
  }

  /** Changes a participant's metadata, name, attributes or permissions. */
  async updateParticipant(room: string, identity: string, options: UpdateParticipantOptions): Promise<ParticipantInfo> {
    const req = create(UpdateParticipantRequestSchema, {
      room,
      identity,
      metadata: options.metadata,
      name: options.name,
      attributes: options.attributes,
      permission: options.permission,
    });
    return fromJson(ParticipantInfoSchema, await this.call('UpdateParticipant', admin(room), toJson(UpdateParticipantRequestSchema, req)), parse);
  }

  /** Subscribes a participant to tracks, or unsubscribes them. */
  async updateSubscriptions(room: string, identity: string, trackSids: string[], subscribe: boolean): Promise<void> {
    const body = toJson(UpdateSubscriptionsRequestSchema, create(UpdateSubscriptionsRequestSchema, { room, identity, trackSids, subscribe }));
    await this.call('UpdateSubscriptions', admin(room), body);
  }

  /** Sends a data packet to a room, or to the identities named. */
  async sendData(room: string, data: Uint8Array, kind: DataPacket_Kind = DataPacket_Kind.RELIABLE, options: SendDataOptions = {}): Promise<void> {
    const req = create(SendDataRequestSchema, {
      room,
      data,
      kind,
      destinationIdentities: options.destinationIdentities ?? [],
      topic: options.topic,
      // Lets a client spot a packet it has already seen, when a call is retried.
      nonce: crypto.getRandomValues(new Uint8Array(16)),
    });
    await this.call('SendData', admin(room), toJson(SendDataRequestSchema, req));
  }

  /** Not implemented by this server, or by the open-source reference server. */
  async forwardParticipant(room: string, identity: string, destinationRoom: string): Promise<void> {
    const body = toJson(ForwardParticipantRequestSchema, create(ForwardParticipantRequestSchema, { room, identity, destinationRoom }));
    await this.call('ForwardParticipant', { roomAdmin: true, room, destinationRoom }, body);
  }

  /** Not implemented by this server, or by the open-source reference server. */
  async moveParticipant(room: string, identity: string, destinationRoom: string): Promise<void> {
    const body = toJson(MoveParticipantRequestSchema, create(MoveParticipantRequestSchema, { room, identity, destinationRoom }));
    await this.call('MoveParticipant', { roomAdmin: true, room, destinationRoom }, body);
  }

  /** Asks a participant to run a method. Not implemented by this server yet: it fails with the code unimplemented. */
  async performRpc(room: string, destinationIdentity: string, method: string, payload: string, responseTimeoutMs = 0): Promise<string> {
    const body = toJson(PerformRpcRequestSchema, create(PerformRpcRequestSchema, { room, destinationIdentity, method, payload, responseTimeoutMs }));
    return fromJson(PerformRpcResponseSchema, await this.call('PerformRpc', admin(room), body), parse).payload;
  }
}
