import { Transport, camelCased, pageQuery, type ClientOptions } from './http.js';

export type IngressInput = 'rtmp' | 'whip' | 'url';
export type IngressStatus = 'inactive' | 'buffering' | 'publishing' | 'error';

/** Describes a running stream. */
export interface IngressStats {
  bitrateIn: number;
  width: number;
  height: number;
  fps: number;
  audioCodec: string;
  videoCodec: string;
  startedAt: string | null;
}

/** A way to bring outside media into a room. Times are RFC 3339. */
export interface Ingress {
  id: string;
  projectId: string;
  name: string;
  inputType: IngressInput;
  roomName: string;
  participantIdentity: string;
  participantName: string;
  sourceUrl: string;
  enabled: boolean;
  /** Where a publisher sends its stream. */
  publishUrl: string;
  /** Only in the responses to createIngress and rotateIngressKey. Keep it secret. */
  streamKey?: string;
  status: IngressStatus;
  error: string;
  stats: IngressStats;
  createdAt: string;
  updatedAt: string;
}

export interface CreateIngressOptions {
  name?: string;
  room: string;
  participantIdentity: string;
  participantName?: string;
  /** What a url input pulls from: http, https, rtmp or srt. */
  sourceUrl?: string;
}

/** Changes to an ingress. Changing the room or identity, or disabling it, ends a running stream. */
export interface UpdateIngressOptions {
  name?: string;
  room?: string;
  participantIdentity?: string;
  participantName?: string;
  enabled?: boolean;
}

export interface IngressEvent {
  level: string;
  type: string;
  message: string;
  createdAt: string;
}

const ADMIN = { ingressAdmin: true };

/**
 * Manages ingresses, over this server's REST API. The server needs a control plane and, for a stream to actually arrive,
 * an ingress worker.
 */
export class IngressClient {
  private readonly transport: Transport;

  constructor(url: string, apiKey?: string, apiSecret?: string, options?: ClientOptions) {
    this.transport = new Transport(url, apiKey, apiSecret, options);
  }

  private async one(verb: string, path: string, body?: unknown): Promise<Ingress> {
    return camelCased<Ingress>(await (await this.transport.rest(verb, path, ADMIN, body)).json());
  }

  /** Creates an ingress. The response holds the stream key, which is never shown again. */
  createIngress(inputType: IngressInput, options: CreateIngressOptions): Promise<Ingress> {
    return this.one('POST', '/ingress', {
      input_type: inputType,
      name: options.name,
      room: options.room,
      participant_identity: options.participantIdentity,
      participant_name: options.participantName,
      source_url: options.sourceUrl,
    });
  }

  /** Lists ingresses. */
  async listIngress(): Promise<Ingress[]> {
    const res = await this.transport.rest('GET', '/ingress', ADMIN);
    return camelCased<{ items: Ingress[] }>(await res.json()).items;
  }

  /** Returns one ingress. */
  getIngress(id: string): Promise<Ingress> {
    return this.one('GET', `/ingress/${encodeURIComponent(id)}`);
  }

  /** Changes an ingress. Fields left out stay as they are. */
  updateIngress(id: string, options: UpdateIngressOptions): Promise<Ingress> {
    return this.one('PATCH', `/ingress/${encodeURIComponent(id)}`, {
      name: options.name,
      room: options.room,
      participant_identity: options.participantIdentity,
      participant_name: options.participantName,
      enabled: options.enabled,
    });
  }

  /** Deletes an ingress and stops any running stream. */
  async deleteIngress(id: string): Promise<void> {
    await this.transport.rest('DELETE', `/ingress/${encodeURIComponent(id)}`, ADMIN);
  }

  /** Stops the running stream. The ingress can be used again. */
  stopIngress(id: string): Promise<Ingress> {
    return this.one('POST', `/ingress/${encodeURIComponent(id)}/stop`);
  }

  /** Replaces the stream key, and returns the new one once. The old one stops working and its stream ends. */
  rotateIngressKey(id: string): Promise<Ingress> {
    return this.one('POST', `/ingress/${encodeURIComponent(id)}/rotate-key`);
  }

  /** Returns an ingress's events, newest first. */
  async ingressLogs(id: string, limit?: number): Promise<IngressEvent[]> {
    const res = await this.transport.rest('GET', `/ingress/${encodeURIComponent(id)}/logs${pageQuery(limit)}`, ADMIN);
    return camelCased<{ items: IngressEvent[] }>(await res.json()).items;
  }
}
