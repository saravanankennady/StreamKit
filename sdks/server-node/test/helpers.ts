import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { afterEach, describe, inject } from 'vitest';
import { StreamKitAPI } from '../src/index.js';
import type { FixtureInfo, Server } from './global-setup.js';

export type { FixtureInfo, Server };

export const fixture = () => inject('fixture') as FixtureInfo | null;

/** One line of `streamkit room join --json`: something a real participant saw. */
export interface PeerEvent {
  event: string;
  room?: string;
  metadata?: string;
  identity?: string;
  sid?: string;
  name?: string;
  attributes?: Record<string, string>;
  can_publish?: boolean;
  kind?: string;
  source?: string;
  muted?: boolean;
  packets?: number;
  bytes?: number;
  topic?: string;
  channel?: string;
  from?: string;
  data_base64?: string;
  reason?: string;
  error?: string;
}

let counter = 0;
export const roomName = (tag: string) => `sdk-node-${tag}-${process.pid}-${++counter}`;
export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const configDir = mkdtempSync(join(tmpdir(), 'sdk-node-peers-'));

/** A real participant, run as the streamkit CLI: a real WebRTC session with real media. */
export class Peer {
  readonly events: PeerEvent[] = [];
  readonly identity: string;
  private readonly child: ChildProcess;
  private stderr = '';
  private closed = false;
  private readonly waiters = new Set<() => void>();

  constructor(cli: string, server: Server, room: string, identity: string, options: { publish?: boolean; noAutoSubscribe?: boolean; token?: string } = {}) {
    this.identity = identity;
    const args = ['room', 'join', room, '--json', '--url', server.url, '--identity', identity];
    if (options.token) args.push('--token', options.token);
    else args.push('--api-key', server.api_key, '--api-secret', server.api_secret);
    if (options.publish) args.push('--publish-test-pattern');
    if (options.noAutoSubscribe) args.push('--no-auto-subscribe');
    this.child = spawn(cli, args, { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, STREAMKIT_CONFIG_DIR: configDir } });
    this.child.stderr!.on('data', (d) => (this.stderr += d));
    createInterface({ input: this.child.stdout! }).on('line', (line) => {
      try {
        this.events.push(JSON.parse(line));
      } catch {
        return;
      }
      this.wake();
    });
    this.child.on('exit', () => {
      this.closed = true;
      this.wake();
    });
  }

  private wake() {
    for (const w of this.waiters) w();
  }

  /** The first event since the start of the session that check accepts. */
  async wait(what: string, check: (e: PeerEvent) => boolean, timeoutMs = 30000): Promise<PeerEvent> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const found = this.events.find(check);
      if (found) return found;
      const saw = () => this.events.filter((e) => e.event !== 'media').map((e) => e.event).join(',');
      if (this.closed) throw new Error(`${this.identity} left without ${what}. Saw ${saw()}. Errors: ${this.stderr}`);
      const remaining = deadline - Date.now();
      if (remaining <= 0) throw new Error(`${this.identity}: timed out waiting for ${what}. Saw ${saw()}. Errors: ${this.stderr}`);
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          this.waiters.delete(done);
          resolve();
        };
        const timer = setTimeout(done, remaining);
        this.waiters.add(done);
      });
    }
  }

  connected() {
    return this.wait('a connection', (e) => e.event === 'connected');
  }

  async dataChannels() {
    for (const label of ['_reliable', '_lossy']) {
      await this.wait(`data channel ${label}`, (e) => e.event === 'data_channel_open' && e.channel === label);
    }
  }

  /** Waits until the server has accepted the test pattern's two tracks. */
  async published(): Promise<PeerEvent[]> {
    const deadline = Date.now() + 30000;
    while (Date.now() < deadline) {
      const tracks = this.events.filter((e) => e.event === 'track_published');
      if (tracks.length >= 2) return tracks;
      await this.wait('a change', () => false, 200).catch(() => {});
    }
    throw new Error(`${this.identity}: the tracks were never published`);
  }

  async close() {
    if (this.closed) return;
    const exited = new Promise((resolve) => this.child.once('exit', resolve));
    this.child.kill('SIGTERM');
    await exited;
  }
}

/** What each test needs from the server it runs against. */
export interface Target {
  name: 'ours' | 'reference';
  server: Server;
  api: StreamKitAPI;
  join: (room: string, identity: string, options?: { publish?: boolean; noAutoSubscribe?: boolean; token?: string }) => Peer;
}

/** Declares a block of tests against this server and the reference. A test the reference fails tests the wrong thing. */
export function eachServer(name: string, body: (target: () => Target) => void) {
  const info = fixture();
  for (const which of ['ours', 'reference'] as const) {
    const server = info?.[which] ?? null;
    describe.skipIf(!server)(`${name} [${which}]`, () => {
      const peers: Peer[] = [];
      afterEach(async () => {
        await Promise.all(peers.splice(0).map((p) => p.close()));
      });
      body(() => ({
        name: which,
        server: server!,
        api: new StreamKitAPI(server!.url, server!.api_key, server!.api_secret),
        join: (room, identity, options) => {
          const peer = new Peer(info!.cli, server!, room, identity, options);
          peers.push(peer);
          return peer;
        },
      }));
    });
  }
}

export const dataEvent = (payload: string) => (e: PeerEvent) => e.event === 'data' && e.data_base64 === btoa(payload);

/** A webhook as it arrived at the capture server. */
export interface Captured {
  authorization: string;
  content_type: string;
  user_agent: string;
  body: string;
}

export async function capturedWebhooks(which: string): Promise<Captured[]> {
  const res = await fetch(`${fixture()!.webhooks}/captured/${which}`);
  return res.json() as Promise<Captured[]>;
}
