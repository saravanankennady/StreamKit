import { fromJson, toJson, type JsonValue } from '@bufbuild/protobuf';
import * as jose from 'jose';
import type {
  AgentEndpointGrant,
  AgentGrant,
  AgentThreadGrant,
  ClaimGrants,
  InferenceGrant,
  ObservabilityGrant,
  SIPGrant,
  VideoGrant,
} from './grants.js';
import { RoomConfigurationSchema, type RoomConfiguration } from './proto/livekit_room_pb.js';

const DEFAULT_TTL = '6h';
/** Clock difference tolerated between whoever signed a token and whoever checks it. Servers allow the same. */
const LEEWAY_SECONDS = 60;

export interface AccessTokenOptions {
  /** How long the token lasts: seconds, or a span such as '10m' or '2 days'. The default is six hours. */
  ttl?: number | string;
  /** Who holds the token. A token that joins a room needs one. */
  identity?: string;
  /** The participant's display name. */
  name?: string;
  /** The participant's metadata. */
  metadata?: string;
  /** The participant's attributes. */
  attributes?: Record<string, string>;
}

/** Builds a signed token: an HS256 JWT with the API key as its issuer. */
export class AccessToken {
  private readonly apiKey: string;
  private readonly apiSecret: string;
  private readonly claims: Omit<ClaimGrants, 'identity'> = {};

  identity?: string;
  ttl: number | string;

  /**
   * @param apiKey - the API key, or STREAMKIT_API_KEY
   * @param apiSecret - the API secret, or STREAMKIT_API_SECRET
   */
  constructor(apiKey?: string, apiSecret?: string, options: AccessTokenOptions = {}) {
    // A secret in a page is a secret anyone can read, so this refuses instead of warning.
    if (typeof (globalThis as { document?: unknown }).document !== 'undefined') {
      throw new Error('Access tokens must be made on a server. An API secret must never reach a browser.');
    }
    const key = apiKey ?? process.env.STREAMKIT_API_KEY;
    const secret = apiSecret ?? process.env.STREAMKIT_API_SECRET;
    if (!key || !secret) {
      throw new Error('An API key and secret are required.');
    }
    this.apiKey = key;
    this.apiSecret = secret;
    this.identity = options.identity;
    this.ttl = options.ttl ?? DEFAULT_TTL;
    if (options.name) this.claims.name = options.name;
    if (options.metadata) this.claims.metadata = options.metadata;
    if (options.attributes) this.claims.attributes = options.attributes;
  }

  /** Adds room permissions, merged with any already added. */
  addGrant(grant: VideoGrant): this {
    this.claims.video = { ...this.claims.video, ...grant };
    return this;
  }

  addSIPGrant(grant: SIPGrant): this {
    this.claims.sip = { ...this.claims.sip, ...grant };
    return this;
  }

  addAgentGrant(grant: AgentGrant): this {
    this.claims.agent = { ...this.claims.agent, ...grant };
    return this;
  }

  addInferenceGrant(grant: InferenceGrant): this {
    this.claims.inference = { ...this.claims.inference, ...grant };
    return this;
  }

  addObservabilityGrant(grant: ObservabilityGrant): this {
    this.claims.observability = { ...this.claims.observability, ...grant };
    return this;
  }

  addAgentEndpointGrant(grant: AgentEndpointGrant): this {
    this.claims.agentEndpoint = { ...this.claims.agentEndpoint, ...grant };
    return this;
  }

  addAgentThreadGrant(grant: AgentThreadGrant): this {
    this.claims.agentThread = { ...this.claims.agentThread, ...grant };
    return this;
  }

  get name(): string | undefined {
    return this.claims.name;
  }
  set name(name: string | undefined) {
    this.claims.name = name;
  }

  get metadata(): string | undefined {
    return this.claims.metadata;
  }
  set metadata(metadata: string | undefined) {
    this.claims.metadata = metadata;
  }

  get attributes(): Record<string, string> | undefined {
    return this.claims.attributes;
  }
  set attributes(attributes: Record<string, string> | undefined) {
    this.claims.attributes = attributes;
  }

  get kind(): string | undefined {
    return this.claims.kind;
  }
  set kind(kind: string | undefined) {
    this.claims.kind = kind;
  }

  get kindDetails(): string[] | undefined {
    return this.claims.kindDetails;
  }
  set kindDetails(details: string[] | undefined) {
    this.claims.kindDetails = details;
  }

  get sha256(): string | undefined {
    return this.claims.sha256;
  }
  set sha256(sha256: string | undefined) {
    this.claims.sha256 = sha256;
  }

  get roomPreset(): string | undefined {
    return this.claims.roomPreset;
  }
  set roomPreset(preset: string | undefined) {
    this.claims.roomPreset = preset;
  }

  get roomConfig(): RoomConfiguration | undefined {
    return this.claims.roomConfig;
  }
  set roomConfig(config: RoomConfiguration | undefined) {
    this.claims.roomConfig = config;
  }

  /** Signs the token. */
  async toJwt(): Promise<string> {
    const video = this.claims.video;
    if (video?.roomJoin && (!this.identity || !video.room)) {
      throw new Error('A token that joins a room needs an identity and a room.');
    }
    const { roomConfig, ...rest } = this.claims;
    const payload: Record<string, unknown> = { ...rest };
    if (roomConfig) {
      payload.roomConfig = toJson(RoomConfigurationSchema, roomConfig);
    }
    const now = Math.floor(Date.now() / 1000);
    const expires = typeof this.ttl === 'number' ? now + this.ttl : this.ttl;
    const jwt = new jose.SignJWT(payload)
      .setProtectedHeader({ alg: 'HS256' })
      .setIssuer(this.apiKey)
      .setIssuedAt(now)
      .setNotBefore(now)
      .setExpirationTime(expires);
    if (this.identity) {
      jwt.setSubject(this.identity);
    }
    return jwt.sign(new TextEncoder().encode(this.apiSecret));
  }
}

/** Checks tokens signed with one key, the same checks a server makes. */
export class TokenVerifier {
  private readonly apiKey: string;
  private readonly apiSecret: string;

  constructor(apiKey: string, apiSecret: string) {
    this.apiKey = apiKey;
    this.apiSecret = apiSecret;
  }

  /**
   * Checks a token's signature, issuer and validity window, and returns its claims. It takes HS256 only, requires an
   * expiry, and ignores claims it does not know. It throws if the token fails.
   */
  async verify(token: string): Promise<ClaimGrants> {
    const { payload } = await jose.jwtVerify(token, new TextEncoder().encode(this.apiSecret), {
      algorithms: ['HS256'],
      issuer: this.apiKey,
      clockTolerance: LEEWAY_SECONDS,
      requiredClaims: ['exp'],
    });
    const { sub, jti, roomConfig, ...rest } = payload as jose.JWTPayload & { roomConfig?: JsonValue };
    const claims: ClaimGrants = { ...(rest as ClaimGrants), identity: sub ?? jti };
    if (roomConfig !== undefined) {
      claims.roomConfig = fromJson(RoomConfigurationSchema, roomConfig, { ignoreUnknownFields: true });
    }
    return claims;
  }
}
