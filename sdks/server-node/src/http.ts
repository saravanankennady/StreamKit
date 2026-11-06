import { AccessToken } from './AccessToken.js';
import type { VideoGrant } from './grants.js';

const USER_AGENT = 'elnova-streamkit-server-node/0.1.0';
/** How long the token signed for each call lasts. */
const CALL_TOKEN_TTL_SECONDS = 600;
const DEFAULT_TIMEOUT_SECONDS = 30;

export interface ClientOptions {
  /** How long a call may take, in seconds. The default is 30. */
  requestTimeout?: number;
  /**
   * Send this token on every call, instead of signing one per call with only the grants that call needs. You are then
   * responsible for its grants.
   */
  token?: string;
}

/** A failure the server reported. */
export class ServerError extends Error {
  /** The HTTP status. */
  readonly status: number;
  /** The server's error code, such as not_found or unauthenticated. */
  readonly code: string;
  /** Extra Twirp detail, when the server gave any. */
  readonly metadata?: Record<string, string>;
  /** The fields that failed validation, on the REST API. */
  readonly fields?: Record<string, string>;

  constructor(code: string, message: string, status: number, extra: { metadata?: Record<string, string>; fields?: Record<string, string> } = {}) {
    super(message);
    this.name = 'ServerError';
    this.code = code;
    this.status = status;
    this.metadata = extra.metadata;
    this.fields = extra.fields;
  }
}

/** Something this server does not do. */
export class NotSupportedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'NotSupportedError';
  }
}

function codeForStatus(status: number): string {
  switch (status) {
    case 400:
      return 'invalid_argument';
    case 401:
      return 'unauthenticated';
    case 403:
      return 'permission_denied';
    case 404:
      return 'not_found';
    case 429:
      return 'resource_exhausted';
    case 501:
      return 'unimplemented';
    case 502:
    case 503:
    case 504:
      return 'unavailable';
    default:
      return 'unknown';
  }
}

/** Reads a failure. Twirp errors are {code,msg}, REST errors are {error:{code,message}}, and the rest is plain text. */
async function serverError(res: Response): Promise<ServerError> {
  const text = await res.text();
  let body: { code?: string; msg?: string; meta?: Record<string, string>; error?: { code?: string; message?: string; fields?: Record<string, string> } } | undefined;
  try {
    body = JSON.parse(text);
  } catch {
    // Authentication failures and proxies answer in plain text.
  }
  if (body?.error?.code) {
    return new ServerError(body.error.code, body.error.message ?? '', res.status, { fields: body.error.fields });
  }
  if (body?.code) {
    return new ServerError(body.code, body.msg ?? '', res.status, { metadata: body.meta });
  }
  return new ServerError(codeForStatus(res.status), text.trim(), res.status);
}

function isJson(res: Response): boolean {
  return (res.headers.get('content-type') ?? '').startsWith('application/json');
}

/** What every client shares: where the server is, and how to sign for it. */
export class Transport {
  readonly url: string;
  private readonly apiKey?: string;
  private readonly apiSecret?: string;
  private readonly options: ClientOptions;

  /** @param url - the server, which may be the ws:// address clients connect to */
  constructor(url: string, apiKey?: string, apiSecret?: string, options: ClientOptions = {}) {
    this.url = url.replace(/^ws/, 'http').replace(/\/+$/, '');
    this.apiKey = apiKey;
    this.apiSecret = apiSecret;
    this.options = options;
  }

  private async authorization(grant: VideoGrant): Promise<string> {
    if (this.options.token) {
      return `Bearer ${this.options.token}`;
    }
    const token = new AccessToken(this.apiKey, this.apiSecret, { ttl: CALL_TOKEN_TTL_SECONDS });
    token.addGrant(grant);
    return `Bearer ${await token.toJwt()}`;
  }

  private signal(): AbortSignal {
    return AbortSignal.timeout((this.options.requestTimeout ?? DEFAULT_TIMEOUT_SECONDS) * 1000);
  }

  /** Calls one Twirp method in JSON. A route that does not exist means the server does not have this service. */
  async twirp(service: string, method: string, grant: VideoGrant, body: unknown): Promise<unknown> {
    const res = await fetch(`${this.url}/twirp/livekit.${service}/${method}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'User-Agent': USER_AGENT, Authorization: await this.authorization(grant) },
      body: JSON.stringify(body),
      signal: this.signal(),
    });
    if (res.ok) {
      return res.json();
    }
    if (res.status === 404 && !isJson(res)) {
      await res.body?.cancel();
      throw new NotSupportedError(`${method}: this server has no ${service} service.`);
    }
    throw await serverError(res);
  }

  /** Calls the management API. A download has no time limit, because reading its body can take far longer than a call. */
  async rest(verb: string, path: string, grant: VideoGrant, body?: unknown, options: { download?: boolean } = {}): Promise<Response> {
    const res = await fetch(`${this.url}/api/v1${path}`, {
      method: verb,
      headers: { 'Content-Type': 'application/json', 'User-Agent': USER_AGENT, Authorization: await this.authorization(grant) },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: options.download ? undefined : this.signal(),
    });
    if (res.ok) {
      return res;
    }
    if (res.status === 404 && !isJson(res)) {
      await res.body?.cancel();
      throw new NotSupportedError(
        `${this.url} has no management API at /api/v1. It needs an Elnova StreamKit server with a control plane (control_plane.database_url).`,
      );
    }
    throw await serverError(res);
  }
}

/** Turns the REST API's snake_case names into the camelCase this SDK's types use. */
export function camelCased<T>(value: unknown): T {
  if (Array.isArray(value)) {
    return value.map((v) => camelCased(v)) as T;
  }
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value).map(([k, v]) => [k.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase()), camelCased(v)]),
    ) as T;
  }
  return value as T;
}

export function pageQuery(limit?: number, cursor?: string): string {
  const q = new URLSearchParams();
  if (limit) q.set('limit', String(limit));
  if (cursor) q.set('cursor', cursor);
  const text = q.toString();
  return text ? `?${text}` : '';
}
