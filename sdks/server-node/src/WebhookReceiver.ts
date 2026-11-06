import { fromJson } from '@bufbuild/protobuf';
import { AccessToken, TokenVerifier } from './AccessToken.js';
import { WebhookEventSchema, type WebhookEvent } from './proto/livekit_webhook_pb.js';

/** How long a signature made by {@link signWebhook} lasts: the same as a server's. */
const SIGNATURE_TTL_SECONDS = 300;

async function bodyHash(body: Uint8Array): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', body as Uint8Array<ArrayBuffer>));
  return btoa(String.fromCharCode(...digest));
}

function equalInConstantTime(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let difference = 0;
  for (let i = 0; i < a.length; i++) {
    difference |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return difference === 0;
}

/** Checks that webhooks really came from a server holding this key. */
export class WebhookReceiver {
  private readonly verifier: TokenVerifier;

  constructor(apiKey: string, apiSecret: string) {
    this.verifier = new TokenVerifier(apiKey, apiSecret);
  }

  /**
   * Checks a webhook and returns its event. It throws if the check fails.
   *
   * Pass the body as it arrived. Parsing it first and serializing it again changes the bytes, and the signature covers
   * the bytes. Fields this SDK does not know are ignored.
   *
   * @param body - the request body
   * @param authorization - the Authorization header, which is a token with no Bearer prefix
   */
  async receive(body: string | Uint8Array, authorization: string | undefined): Promise<WebhookEvent> {
    if (!authorization) {
      throw new Error('The webhook has no Authorization header.');
    }
    const claims = await this.verifier.verify(authorization);
    const bytes = typeof body === 'string' ? new TextEncoder().encode(body) : body;
    if (!claims.sha256 || !equalInConstantTime(claims.sha256, await bodyHash(bytes))) {
      throw new Error('The webhook body does not match its signature.');
    }
    return fromJson(WebhookEventSchema, JSON.parse(new TextDecoder().decode(bytes)), { ignoreUnknownFields: true });
  }
}

/** Returns the Authorization header value for a webhook with this body. Servers do this themselves: it is for testing a receiver. */
export async function signWebhook(body: string | Uint8Array, apiKey: string, apiSecret: string): Promise<string> {
  const bytes = typeof body === 'string' ? new TextEncoder().encode(body) : body;
  const token = new AccessToken(apiKey, apiSecret, { ttl: SIGNATURE_TTL_SECONDS });
  token.sha256 = await bodyHash(bytes);
  return token.toJwt();
}
