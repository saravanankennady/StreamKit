// Response shapes of the REST API, from the Go structs in api/store, server/monitor and api/routes, and from
// docs/api/media-services.md for the egress, recording and ingress routes.

export interface User {
  id: string;
  email: string;
  name: string;
  is_platform_admin: boolean;
  email_verified_at: string | null;
  created_at: string;
}

export type OrgRole = "owner" | "admin" | "member";

export interface Organization {
  id: string;
  name: string;
  role?: OrgRole;
  created_at: string;
}

export interface IceServer {
  urls: string[];
  username?: string;
  credential?: string;
}

export interface ProjectSettings {
  empty_timeout?: number;
  departure_timeout?: number;
  max_participants?: number;
  auto_create_rooms?: boolean;
  allowed_origins?: string[];
  token_ttl_seconds?: number;
  ice_servers?: IceServer[];
}

export type Environment = "development" | "staging" | "production";

export interface Project {
  id: string;
  org_id: string;
  name: string;
  region: string;
  environment: Environment;
  status: "active" | "suspended";
  settings: ProjectSettings;
  created_at: string;
  updated_at: string;
  url: string;
}

export interface Me {
  user: User;
  organizations: Organization[];
  projects: Project[];
}

export interface Session {
  id: string;
  user_agent: string;
  ip: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  current: boolean;
}

export interface ApiKey {
  id: string;
  project_id: string;
  api_key: string;
  name: string;
  created_at: string;
  expires_at: string | null;
  revoked_at: string | null;
  secret?: string;
}

export type RoomState = "creating" | "active" | "empty" | "closing" | "closed" | "failed";

export interface JoinLink {
  id: string;
  project_id: string;
  room: string;
  label: string;
  can_publish: boolean;
  role: "guest" | "host";
  uses: number;
  created_at: string;
  expires_at: string;
  url: string;
}

export interface RoomStat {
  sid: string;
  name: string;
  state: RoomState;
  metadata: string;
  participants: number;
  max_participants: number;
  publishers: number;
  audio_streams: number;
  video_streams: number;
  created_at: number;
  duration_seconds: number;
  bitrate_in: number;
  bitrate_out: number;
  empty_timeout: number;
  departure_timeout: number;
  active_recording: boolean;
}

export interface RoomRecord {
  sid: string;
  project_id: string;
  name: string;
  node_id: string;
  metadata: string;
  max_participants: number;
  peak_participants: number;
  total_participants: number;
  created_at: string;
  ended_at: string | null;
}

export interface TrackStat {
  sid: string;
  kind: "audio" | "video";
  source: string;
  mime_type: string;
  muted: boolean;
  simulcast: boolean;
  width: number;
  height: number;
  fps: number;
  bitrate_in: number;
  layers: number;
  packets_in: number;
  packets_lost: number;
  subscribers: number;
}

/** Encoded by Go's encoding/json from the protobuf struct: false fields are left out, sources are enum numbers. */
export interface ParticipantPermission {
  can_subscribe?: boolean;
  can_publish?: boolean;
  can_publish_data?: boolean;
  can_publish_sources?: number[];
  hidden?: boolean;
  can_update_metadata?: boolean;
}

export interface ParticipantStat {
  sid: string;
  identity: string;
  name: string;
  room: string;
  state: string;
  kind: string;
  joined_at: number;
  duration_seconds: number;
  quality: string;
  quality_score: number;
  tracks: TrackStat[];
  subscribed_count: number;
  has_audio: boolean;
  has_video: boolean;
  audio_muted: boolean;
  video_muted: boolean;
  bitrate_in: number;
  bitrate_out: number;
  bandwidth_estimate: number;
  rtt_ms: number;
  jitter_ms: number;
  loss_ratio: number;
  packets_in: number;
  packets_out: number;
  permission?: ParticipantPermission;
}

export interface NodeStats {
  node_id: string;
  hostname: string;
  cpu_percent: number;
  process_cpu_percent: number;
  num_cpu: number;
  memory_used: number;
  memory_total: number;
  process_memory: number;
  goroutines: number;
  uptime_seconds: number;
  draining: boolean;
  healthy: boolean;
}

export interface ProjectSnapshot {
  at: string;
  active_rooms: number;
  participants: number;
  connections: number;
  audio_streams: number;
  video_streams: number;
  subscriptions: number;
  bitrate_in: number;
  bitrate_out: number;
  packets_in_rate: number;
  packets_out_rate: number;
  packet_loss_ratio: number;
  avg_rtt_ms: number;
  avg_jitter_ms: number;
  rooms: RoomStat[];
  participant_stats: ParticipantStat[];
  node: NodeStats;
}

export interface Webhook {
  id: string;
  project_id: string;
  url: string;
  api_key_id: string;
  api_key: string;
  events: string[];
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export type DeliveryStatus = "pending" | "delivered" | "failed";

export interface Delivery {
  id: string;
  webhook_id: string;
  event_type: string;
  payload: unknown;
  status: DeliveryStatus;
  attempts: number;
  response_status: number | null;
  last_error: string;
  created_at: string;
  next_attempt_at: string;
  delivered_at: string | null;
}

export interface UsagePoint {
  bucket: string;
  participant_minutes: number;
  room_minutes: number;
  audio_minutes: number;
  video_minutes: number;
  recording_minutes: number;
  bytes_in: number;
  bytes_out: number;
  packets_in: number;
  packets_lost: number;
  connection_failures: number;
  peak_participants: number;
  peak_rooms: number;
  avg_rtt_ms: number;
}

export interface AnalyticsOverview {
  from: string;
  to: string;
  total_rooms: number;
  avg_room_seconds: number;
  totals: UsagePoint;
}

export type WorkerKind = "egress_worker" | "ingress_worker";

/** Media servers report rooms and participants. Media workers report the same table, with their kind and jobs instead. */
export interface ServerStats {
  node_id: string;
  hostname: string;
  cpu_percent: number;
  num_cpu: number;
  memory_used: number;
  memory_total: number;
  uptime_seconds: number;
  draining: boolean;
  healthy: boolean;
  process_cpu_percent?: number;
  process_memory?: number;
  goroutines?: number;
  active_rooms?: number;
  participants?: number;
  kind?: WorkerKind;
  max_jobs?: number;
  active_jobs?: number;
  /** What a worker says is wrong, when healthy is false. Only platform admins get this. */
  unhealthy_reason?: string;
}

export interface ServerNode {
  node_id: string;
  region: string;
  version: string;
  address: string;
  started_at: string;
  last_seen_at: string;
  draining: boolean;
  stats: ServerStats;
}

export interface LogEvent {
  id: number;
  project_id: string | null;
  category: string;
  level: "info" | "warn" | "error";
  type: string;
  message: string;
  room_name: string;
  room_sid: string;
  participant_identity: string;
  track_sid: string;
  details: unknown;
  created_at: string;
}

export interface AuditEntry {
  id: number;
  project_id: string | null;
  user_id: string | null;
  user_email: string | null;
  action: string;
  target: string;
  ip: string;
  details: unknown;
  created_at: string;
}

export interface Page<T> {
  items: T[];
  next_cursor?: string;
}

export interface Items<T> {
  items: T[];
}

export type EgressStatus = "starting" | "active" | "ending" | "complete" | "failed" | "aborted";
export type EgressType = "room_composite" | "participant" | "audio_only" | "video_only";

export interface EgressJob {
  id: string;
  project_id: string;
  room_name: string;
  room_sid: string;
  type: EgressType;
  status: EgressStatus;
  format: string;
  output_url: string;
  stream_urls: string[];
  file_size: number;
  duration_ms: number;
  error: string;
  created_at: string;
  started_at: string | null;
  ended_at: string | null;
  retry_of: string | null;
}

export type IngressStatus = "inactive" | "buffering" | "publishing" | "error";
export type IngressInput = "rtmp" | "whip" | "url";

export interface IngressStats {
  bitrate_in: number;
  width: number;
  height: number;
  fps: number;
  audio_codec: string;
  video_codec: string;
  started_at: string | null;
}

export interface Ingress {
  id: string;
  name: string;
  input_type: IngressInput;
  room_name: string;
  participant_identity: string;
  participant_name: string;
  source_url: string;
  enabled: boolean;
  publish_url: string;
  stream_key?: string;
  status: IngressStatus;
  error: string;
  stats: IngressStats;
  created_at: string;
  updated_at: string;
}
