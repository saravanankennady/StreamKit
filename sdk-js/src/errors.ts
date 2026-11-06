import { DisconnectReason } from './enums.js';

export enum ConnectionErrorReason {
  /** The server refused this client, usually because of the token. */
  NotAllowed = 'not_allowed',
  ServerUnreachable = 'server_unreachable',
  InternalError = 'internal_error',
  Cancelled = 'cancelled',
  /** The server sent a leave message in place of a join. */
  LeaveRequest = 'leave_request',
  Timeout = 'timeout',
  /** Something answered at the URL, but it does not serve the signalling path. */
  ServiceNotFound = 'service_not_found',
}

export class ConnectionError extends Error {
  readonly reason: ConnectionErrorReason;
  /** The HTTP status behind the failure, if there was one. */
  readonly status?: number;
  /** Why the server refused, when it said so with a leave message. */
  readonly disconnectReason?: DisconnectReason;

  constructor(
    message: string,
    reason: ConnectionErrorReason,
    extra: { status?: number; disconnectReason?: DisconnectReason } = {},
  ) {
    super(message);
    this.name = 'ConnectionError';
    this.reason = reason;
    this.status = extra.status;
    this.disconnectReason = extra.disconnectReason;
  }
}

export class PublishTrackError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'PublishTrackError';
    this.status = status;
  }
}

export class PublishDataError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PublishDataError';
  }
}

export class TrackInvalidError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'TrackInvalidError';
  }
}

export class NegotiationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'NegotiationError';
  }
}

/** A resume failed at the signalling level, so another resume is still worth trying. */
export class SignalReconnectError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'SignalReconnectError';
  }
}

/** Reconnecting cannot succeed from this state, so the session is over. */
export class UnexpectedConnectionState extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'UnexpectedConnectionState';
  }
}

export class SignalRequestError extends Error {
  readonly reason: string;

  constructor(message: string, reason: string) {
    super(message);
    this.name = 'SignalRequestError';
    this.reason = reason;
  }
}
