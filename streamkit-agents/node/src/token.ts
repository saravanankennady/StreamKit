// Signs access tokens, which are HS256 JWTs. A worker needs one to register.
import { createHmac } from 'node:crypto';

function b64(data: Buffer | string): string {
  return Buffer.from(data).toString('base64url');
}

export interface TokenOptions {
  identity?: string;
  video?: Record<string, unknown>;
  name?: string;
  kind?: string;
  metadata?: string;
  attributes?: Record<string, string>;
  roomConfig?: Record<string, unknown>;
  ttlSeconds?: number;
}

export function mintToken(apiKey: string, apiSecret: string, options: TokenOptions = {}): string {
  const now = Math.floor(Date.now() / 1000);
  const claims: Record<string, unknown> = {
    iss: apiKey,
    sub: options.identity ?? '',
    nbf: now,
    iat: now,
    exp: now + (options.ttlSeconds ?? 3600),
    video: options.video ?? {},
  };
  if (options.name) claims.name = options.name;
  if (options.kind) claims.kind = options.kind;
  if (options.metadata) claims.metadata = options.metadata;
  if (options.attributes) claims.attributes = options.attributes;
  if (options.roomConfig) claims.roomConfig = options.roomConfig;
  const header = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const payload = b64(JSON.stringify(claims));
  const signature = createHmac('sha256', apiSecret).update(`${header}.${payload}`).digest();
  return `${header}.${payload}.${b64(signature)}`;
}

/** A token that can do one thing: register a worker. */
export function workerToken(apiKey: string, apiSecret: string): string {
  return mintToken(apiKey, apiSecret, { video: { agent: true } });
}
