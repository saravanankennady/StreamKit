// Real servers and real engines, started for the tests and stopped by PID. Each protocol test runs on our server and the
// reference server. STREAMKIT_TEST_SERVERS=ours or =reference narrows a run to one.
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createWriteStream, existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { AudioFrame, AudioSource, AudioStream, mintToken, rmsLevel, Room, type TextStreamReader } from '../src/index.ts';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '../../..');
const root = process.env.STREAMKIT_DEV_ROOT ?? '/run/media/nsroot/develop/streamin_engine';
const toolchain = join(root, '.toolchain');

export const API_KEY = 'devkey';
export const API_SECRET = 'secret';
const PORTS: Record<string, number> = { ours: 59880, reference: 59980 };
const ENGINE_PORTS = { llm: 57990, stt: 57991, tts: 57992 };

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export async function eventually(condition: () => unknown, message: string, timeoutMs = 15000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${message}`);
    await sleep(50);
  }
}

async function waitHttp(url: string, proc: ChildProcess, timeoutMs = 90000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (proc.exitCode !== null) throw new Error(`${url}: the process exited with ${proc.exitCode}`);
    try {
      if ((await fetch(url)).status === 200) return;
    } catch {
      // not up yet
    }
    await sleep(100);
  }
  throw new Error(`${url} did not come up`);
}

export const wantedServers = (process.env.STREAMKIT_TEST_SERVERS ?? 'ours,reference').split(',').map((s) => s.trim()).filter(Boolean);

export interface RunningServer {
  name: string;
  wsUrl: string;
  httpUrl: string;
  stop: () => Promise<void>;
}

const tmp = mkdtempSync(join(process.env.TMPDIR ?? '/tmp', 'sk-agents-node-'));
process.on('exit', () => rmSync(tmp, { recursive: true, force: true }));

function stopper(proc: ChildProcess): () => Promise<void> {
  return () =>
    new Promise((resolve) => {
      if (proc.exitCode !== null) return resolve();
      proc.once('exit', () => resolve());
      proc.kill('SIGTERM');
    });
}

export async function startServer(name: string): Promise<RunningServer> {
  const port = PORTS[name];
  const log = createWriteStream(join(tmp, `${name}.log`));
  let proc: ChildProcess;
  if (name === 'ours') {
    const binary = join(tmp, 'streamkit-server');
    execFileSync(join(toolchain, 'go/bin/go'), ['build', '-o', binary, './server/cmd/streamkit-server'], {
      cwd: repo,
      env: { ...process.env, GOROOT: join(toolchain, 'go'), GOPATH: join(toolchain, 'gopath'), GOTMPDIR: join(toolchain, 'tmp'), GOTOOLCHAIN: 'local' },
    });
    const config = join(tmp, 'ours.yaml');
    writeFileSync(config, `port: ${port}\nrtc:\n  tcp_port: ${port + 1}\n  udp_port: ${port + 2}\n`);
    proc = spawn(binary, ['--dev', '--config', config]);
  } else {
    const body = `port: ${port}\nrtc:\n  tcp_port: ${port + 1}\n  udp_port: ${port + 2}\n`;
    proc = spawn(join(root, 'reference/oracle/bin/x/livekit-server'), ['--dev', '--bind', '127.0.0.1', '--config-body', body]);
  }
  proc.stdout?.pipe(log);
  proc.stderr?.pipe(log);
  await waitHttp(`http://127.0.0.1:${port}/`, proc);
  return { name, wsUrl: `ws://127.0.0.1:${port}`, httpUrl: `http://127.0.0.1:${port}`, stop: stopper(proc) };
}

export interface Engines {
  llmUrl: string;
  sttUrl: string;
  ttsUrl: string;
  llmModel: string;
  sttModel: string;
  ttsModel: string;
  ttsRate: number;
  stop: () => Promise<void>;
}

export function enginesPresent(): boolean {
  return existsSync(join(toolchain, 'models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf')) && existsSync(join(toolchain, 'venvs/agents-engines'));
}

export async function startEngines(): Promise<Engines> {
  const python = join(toolchain, 'venvs/agents-engines/bin/python');
  const local = join(here, '../../local-engines');
  const llama = join(toolchain, 'llama.cpp/llama-b12102');
  const stt = spawn(python, [join(local, 'stt_server.py'), '--model', join(toolchain, 'models/whisper/base.en'), '--port', String(ENGINE_PORTS.stt), '--threads', '4'], { stdio: 'ignore' });
  const tts = spawn(python, [join(local, 'tts_server.py'), '--voice', join(toolchain, 'models/piper/en_US-ljspeech-medium.onnx'), '--port', String(ENGINE_PORTS.tts)], { stdio: 'ignore' });
  const llm = spawn(join(llama, 'llama-server'), ['-m', join(toolchain, 'models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf'), '--host', '127.0.0.1', '--port', String(ENGINE_PORTS.llm), '-t', '8', '-c', '4096', '--jinja'], {
    stdio: 'ignore',
    env: { ...process.env, LD_LIBRARY_PATH: llama },
  });
  await waitHttp(`http://127.0.0.1:${ENGINE_PORTS.stt}/`, stt);
  await waitHttp(`http://127.0.0.1:${ENGINE_PORTS.tts}/`, tts);
  await waitHttp(`http://127.0.0.1:${ENGINE_PORTS.llm}/health`, llm);
  return {
    llmUrl: `http://127.0.0.1:${ENGINE_PORTS.llm}/v1`,
    sttUrl: `http://127.0.0.1:${ENGINE_PORTS.stt}/v1`,
    ttsUrl: `http://127.0.0.1:${ENGINE_PORTS.tts}/v1`,
    llmModel: 'qwen2.5-1.5b-instruct-q4_k_m',
    sttModel: 'faster-whisper-base.en',
    ttsModel: 'piper-en_US-ljspeech-medium',
    ttsRate: 22050,
    stop: async () => {
      await Promise.all([stopper(stt)(), stopper(tts)(), stopper(llm)()]);
    },
  };
}

export const uniqueRoom = (prefix = 't') => `${prefix}-${Math.random().toString(36).slice(2, 12)}`;

export const adminToken = (room: string) => mintToken(API_KEY, API_SECRET, { identity: 'api', video: { roomAdmin: true, room, roomCreate: true, roomList: true } });

export async function twirp(server: RunningServer, service: string, method: string, body: unknown, token: string): Promise<{ status: number; body: any }> {
  const response = await fetch(`${server.httpUrl}/twirp/livekit.${service}/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  });
  const text = await response.text();
  try {
    return { status: response.status, body: JSON.parse(text) };
  } catch {
    return { status: response.status, body: { raw: text } };
  }
}

/** A person in a room, with a real WebRTC connection. Their microphone plays what the test gives it. */
export class TestUser {
  readonly room = new Room();
  readonly mic = new AudioSource();
  readonly heard = new Map<string, Array<{ at: number; frame: AudioFrame }>>();
  readonly transcripts: Array<{ identity: string; text: string; generated: boolean; finalAttributes: Record<string, string> }> = [];
  private readonly streams: AudioStream[] = [];
  private readonly server: RunningServer;
  private readonly roomName: string;
  private readonly identity: string;

  constructor(server: RunningServer, roomName: string, identity = 'user') {
    this.server = server;
    this.roomName = roomName;
    this.identity = identity;
  }

  async join(publish = true): Promise<void> {
    this.room.on('trackSubscribed', (track, _pub, participant) => {
      const frames = this.heard.get(participant.identity) ?? [];
      this.heard.set(participant.identity, frames);
      this.streams.push(new AudioStream(track, 16000, (frame) => frames.push({ at: Date.now(), frame })));
    });
    this.room.registerTextStreamHandler('lk.transcription', async (reader: TextStreamReader) => {
      const text = await reader.readAll();
      this.transcripts.push({ identity: reader.info.senderIdentity, text, generated: reader.info.generated, finalAttributes: reader.finalAttributes });
    });
    await this.room.connect(this.server.wsUrl, mintToken(API_KEY, API_SECRET, { identity: this.identity, video: { roomJoin: true, room: this.roomName } }));
    if (publish) await this.room.publishTrack(this.mic, { name: 'mic' });
  }

  async say(frame: AudioFrame): Promise<void> {
    const step = Math.floor(frame.sampleRate * 0.05);
    for (let at = 0; at < frame.data.length; at += step) {
      await this.mic.captureFrame(new AudioFrame(frame.data.subarray(at, at + step), frame.sampleRate));
    }
    await this.mic.waitForPlayout();
  }

  /** The audio heard from a participant, as one frame. */
  audioFrom(identity: string): AudioFrame | null {
    const frames = this.heard.get(identity);
    return frames && frames.length ? AudioFrame.concat(frames.map((f) => f.frame)) : null;
  }

  loudFrames(identity: string, threshold = 0.01): number {
    return (this.heard.get(identity) ?? []).filter((f) => rmsLevel(f.frame.data) >= threshold).length;
  }

  async leave(): Promise<void> {
    for (const s of this.streams) s.close();
    this.mic.stop();
    await this.room.disconnect();
  }
}

export function tone(seconds: number, rate = 48000, frequency = 300, amplitude = 0.3): AudioFrame {
  const data = new Int16Array(Math.floor(seconds * rate));
  for (let i = 0; i < data.length; i++) data[i] = Math.sin((2 * Math.PI * frequency * i) / rate) * amplitude * 32767;
  return new AudioFrame(data, rate);
}

/** Waits until the user's audio is reaching the agent. A user's microphone sends silence all the time, so they need not speak. */
export async function agentIsListening(session: () => { framesReceived: number } | null): Promise<void> {
  await eventually(() => (session()?.framesReceived ?? 0) > 0, 'the agent to be listening', 20000);
}
