import { AgentDispatchClient } from './AgentDispatchClient.js';
import { EgressClient } from './EgressClient.js';
import type { ClientOptions } from './http.js';
import { IngressClient } from './IngressClient.js';
import { RoomServiceClient } from './RoomServiceClient.js';

export { AccessToken, TokenVerifier, type AccessTokenOptions } from './AccessToken.js';
export * from './grants.js';
export { WebhookReceiver, signWebhook } from './WebhookReceiver.js';
export { NotSupportedError, ServerError, type ClientOptions } from './http.js';
export { RoomServiceClient, type CreateOptions, type SendDataOptions, type UpdateParticipantOptions } from './RoomServiceClient.js';
export { EgressClient, egressEnded, type Egress, type EgressOutput, type EgressPage, type EgressStatus, type EgressType, type ListOptions } from './EgressClient.js';
export { IngressClient, type CreateIngressOptions, type Ingress, type IngressEvent, type IngressInput, type IngressStats, type IngressStatus, type UpdateIngressOptions } from './IngressClient.js';
export { AgentDispatchClient } from './AgentDispatchClient.js';

export {
  DataPacket_Kind,
  ParticipantInfo_Kind,
  ParticipantInfo_State,
  RoomSchema,
  TrackSource,
  TrackType,
  type ParticipantInfo,
  type ParticipantPermission,
  type Room,
  type TrackInfo,
} from './proto/livekit_models_pb.js';
export { RoomConfigurationSchema, type RoomConfiguration } from './proto/livekit_room_pb.js';
export { WebhookEventSchema, type WebhookEvent } from './proto/livekit_webhook_pb.js';
export type { AgentDispatch } from './proto/livekit_agent_dispatch_pb.js';

/** Clients for all the services of one server. */
export class StreamKitAPI {
  readonly room: RoomServiceClient;
  readonly egress: EgressClient;
  readonly ingress: IngressClient;
  readonly agentDispatch: AgentDispatchClient;

  /**
   * @param url - the server, which may be the ws:// address clients connect to. STREAMKIT_URL if not given
   * @param apiKey - STREAMKIT_API_KEY if not given
   * @param apiSecret - STREAMKIT_API_SECRET if not given
   */
  constructor(url?: string, apiKey?: string, apiSecret?: string, options?: ClientOptions) {
    const server = url ?? process.env.STREAMKIT_URL;
    if (!server) {
      throw new Error('A server address is required.');
    }
    this.room = new RoomServiceClient(server, apiKey, apiSecret, options);
    this.egress = new EgressClient(server, apiKey, apiSecret, options);
    this.ingress = new IngressClient(server, apiKey, apiSecret, options);
    this.agentDispatch = new AgentDispatchClient(server, apiKey, apiSecret, options);
  }
}
