// The string values are part of the API: applications compare against them and log them.

export enum ConnectionState {
  Disconnected = 'disconnected',
  Connecting = 'connecting',
  Connected = 'connected',
  Reconnecting = 'reconnecting',
  SignalReconnecting = 'signalReconnecting',
}

export enum TrackKind {
  Audio = 'audio',
  Video = 'video',
  Unknown = 'unknown',
}

export enum TrackSource {
  Camera = 'camera',
  Microphone = 'microphone',
  ScreenShare = 'screen_share',
  ScreenShareAudio = 'screen_share_audio',
  Unknown = 'unknown',
}

export enum StreamState {
  Active = 'active',
  Paused = 'paused',
  Unknown = 'unknown',
}

export enum SubscriptionStatus {
  Desired = 'desired',
  Subscribed = 'subscribed',
  Unsubscribed = 'unsubscribed',
}

export enum PermissionStatus {
  Allowed = 'allowed',
  NotAllowed = 'not_allowed',
}

export enum ConnectionQuality {
  Excellent = 'excellent',
  Good = 'good',
  Poor = 'poor',
  Lost = 'lost',
  Unknown = 'unknown',
}

// These numeric values are the wire values. test/enums.test.ts holds them to the generated definitions.

export enum VideoQuality {
  LOW = 0,
  MEDIUM = 1,
  HIGH = 2,
  OFF = 3,
}

export enum DataPacketKind {
  RELIABLE = 0,
  LOSSY = 1,
}

export enum DisconnectReason {
  UNKNOWN_REASON = 0,
  CLIENT_INITIATED = 1,
  DUPLICATE_IDENTITY = 2,
  SERVER_SHUTDOWN = 3,
  PARTICIPANT_REMOVED = 4,
  ROOM_DELETED = 5,
  STATE_MISMATCH = 6,
  JOIN_FAILURE = 7,
  MIGRATION = 8,
  SIGNAL_CLOSE = 9,
  ROOM_CLOSED = 10,
  USER_UNAVAILABLE = 11,
  USER_REJECTED = 12,
  SIP_TRUNK_FAILURE = 13,
  CONNECTION_TIMEOUT = 14,
  MEDIA_FAILURE = 15,
  AGENT_ERROR = 16,
}

export enum SubscriptionError {
  SE_UNKNOWN = 0,
  SE_CODEC_UNSUPPORTED = 1,
  SE_TRACK_NOTFOUND = 2,
}

export enum ParticipantKind {
  STANDARD = 0,
  INGRESS = 1,
  EGRESS = 2,
  SIP = 3,
  AGENT = 4,
  CONNECTOR = 7,
  BRIDGE = 8,
}
