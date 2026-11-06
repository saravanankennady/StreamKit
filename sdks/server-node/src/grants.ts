import type { RoomConfiguration } from './proto/livekit_room_pb.js';

/** A track source, as tokens name it. */
export type TrackSourceName = 'camera' | 'microphone' | 'screen_share' | 'screen_share_audio';

/** A token's room permissions. */
export interface VideoGrant {
  /** May create and delete rooms. */
  roomCreate?: boolean;
  /** May list rooms. */
  roomList?: boolean;
  /** May use recording and stream outputs. */
  roomRecord?: boolean;
  /** May moderate the room named by `room`. */
  roomAdmin?: boolean;
  /** May join the room named by `room`. Needs an identity. */
  roomJoin?: boolean;
  /** The room that `roomJoin` and `roomAdmin` apply to. */
  room?: string;
  /** May publish. Granted when unset. */
  canPublish?: boolean;
  /** May subscribe. Granted when unset. */
  canSubscribe?: boolean;
  /** May send data. Follows `canPublish` when unset. */
  canPublishData?: boolean;
  /** Limits publishing to these sources. Empty allows all of them. It cannot allow what `canPublish` denies. */
  canPublishSources?: TrackSourceName[];
  /** May change their own name and metadata. Denied when unset. */
  canUpdateOwnMetadata?: boolean;
  /** May manage ingress. */
  ingressAdmin?: boolean;
  /** Invisible to other participants. */
  hidden?: boolean;
  /** Counts as a recorder. */
  recorder?: boolean;
  /** May connect as an agent worker. */
  agent?: boolean;
  /** May subscribe to metrics. */
  canSubscribeMetrics?: boolean;
  /** May manage an agent session. */
  canManageAgentSession?: boolean;
  /** The room this token may forward or move a participant to. */
  destinationRoom?: string;
}

/** Permission to manage SIP resources and place calls. This server has no SIP service, so it does not act on this yet. */
export interface SIPGrant {
  admin?: boolean;
  call?: boolean;
}

/** Permission over hosted agents. This server does not act on this yet. */
export interface AgentGrant {
  admin?: boolean;
  simulationAdmin?: boolean;
  databaseAdmin?: boolean;
  dispatchAdmin?: boolean;
}

/** Permission to call hosted inference. This server does not act on this. */
export interface InferenceGrant {
  perform?: boolean;
}

/** Permission to publish observability data. This server does not act on this. */
export interface ObservabilityGrant {
  write?: boolean;
}

/** Permission to call an agent's HTTP endpoints. This server does not act on this. */
export interface AgentEndpointGrant {
  call?: boolean;
  agentName?: string;
  deployment?: string;
}

/** Permission over agent threads. This server does not act on this. */
export interface AgentThreadGrant {
  all?: boolean;
  scope?: string;
  threadIds?: string[];
  list?: boolean;
  create?: boolean;
  write?: boolean;
  read?: boolean;
  delete?: boolean;
}

/** Everything a token says about its holder, apart from the JWT's own fields. */
export interface ClaimGrants {
  /** Who holds the token. On the wire this is `sub`. */
  identity?: string;
  name?: string;
  /** The kind of participant: standard, ingress, egress, sip, agent, connector or bridge. */
  kind?: string;
  kindDetails?: string[];
  video?: VideoGrant;
  sip?: SIPGrant;
  agent?: AgentGrant;
  inference?: InferenceGrant;
  observability?: ObservabilityGrant;
  agentEndpoint?: AgentEndpointGrant;
  agentThread?: AgentThreadGrant;
  /** The configuration a room gets if this token's join creates it. */
  roomConfig?: RoomConfiguration;
  /** Names a stored room configuration. */
  roomPreset?: string;
  sha256?: string;
  metadata?: string;
  attributes?: Record<string, string>;
}
