// Starts the real stack for the SIP tests, on loopback, and holds what they share: an independent phone (pjsua), a raw SIP socket that
// knows nothing but the text of the protocol, and a real browser using the official client library. It only ever stops what it started.
import assert from 'node:assert/strict';
import { execFile, execFileSync, spawn } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { createSocket } from 'node:dgram';
import { createWriteStream, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { build } from 'esbuild';
import { AccessToken, RoomServiceClient, SipClient } from 'livekit-server-sdk';
import pg from 'pg';
import { chromium } from 'playwright-core';

const run = promisify(execFile);
export const here = dirname(fileURLToPath(import.meta.url));
export const repo = join(here, '../..');
// SIP_OFFSET moves every port, the database and the working directory, so two runs on one machine do not meet.
export const OFFSET = Number(process.env.SIP_OFFSET ?? 0);
export const tmp = process.env.SIP_TEST_DIR ?? join(tmpdir(), `streamkit-sip-tests${OFFSET || ''}`);
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Everything binds to loopback. Results from here say nothing about real networks, NAT or carriers.
export const HOST = '127.0.0.1';
const PG_ADMIN = 'postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit';
const MASTER_KEY = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
const DB = `streamkit_sip_tests${OFFSET || ''}`;
const DB_URL = `postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/${DB}?sslmode=disable`;

export const findUp = (rel) => {
  let dir = repo;
  for (;;) {
    const candidate = join(dir, rel);
    if (existsSync(candidate)) return candidate;
    const parent = dirname(dir);
    if (parent === dir) return null;
    dir = parent;
  }
};

export const PJSUA = process.env.PJSUA ?? findUp('.toolchain/pjsip/bin/pjsua');
const REFERENCE_SERVER = findUp('reference/oracle/bin/x/livekit-server');
const ORACLE_IMAGE = 'elnova-sip-oracle:dev';

export const waitFor = async (what, fn, { timeout = 20000, every = 100 } = {}) => {
  const until = Date.now() + timeout;
  let last;
  while (Date.now() < until) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (e) {
      if (e.final) throw e;
      last = e;
    }
    await sleep(every);
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last.message}` : ''}`);
};

const children = new Set();

export function startProcess(name, binary, args, { env, cwd } = {}) {
  const log = createWriteStream(join(tmp, `${name}.log`), { flags: 'a' });
  const child = spawn(binary, args, { cwd: cwd ?? tmp, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, ...env } });
  child.stdout.pipe(log);
  child.stderr.pipe(log);
  child.logName = name;
  child.exited = new Promise((resolve) => child.on('exit', (code, signal) => { children.delete(child); resolve({ code, signal }); }));
  children.add(child);
  return child;
}

export async function stopProcess(child, signal = 'SIGTERM', timeoutMs = 20000) {
  if (!children.has(child)) return;
  child.kill(signal);
  const done = await Promise.race([child.exited, sleep(timeoutMs).then(() => null)]);
  if (!done) {
    child.kill('SIGKILL');
    await child.exited;
  }
}

export const logOf = (name) => {
  const path = join(tmp, `${name}.log`);
  return existsSync(path) ? readFileSync(path, 'utf8') : '';
};

export async function buildBinaries() {
  mkdirSync(join(tmp, 'bin'), { recursive: true });
  const toolchain = dirname(findUp('.toolchain/go/bin/go'));
  const env = { ...process.env, PATH: `${toolchain}:${process.env.PATH}`, GOROOT: dirname(toolchain), GOPATH: join(dirname(dirname(toolchain)), 'gopath'), GOTMPDIR: join(dirname(dirname(toolchain)), 'tmp'), GOTOOLCHAIN: 'local' };
  const flags = process.env.SIP_RACE ? ['-race'] : [];
  await run('go', ['build', ...flags, '-o', join(tmp, 'bin/streamkit-server'), './server/cmd/streamkit-server'], { cwd: repo, env });
  await run('go', ['build', ...flags, '-o', join(tmp, 'bin/streamkit-sip'), './sip/cmd/streamkit-sip'], { cwd: repo, env });
}

async function resetDatabase() {
  const admin = new pg.Client({ connectionString: PG_ADMIN });
  await admin.connect();
  await admin.query(`DROP DATABASE IF EXISTS ${DB} WITH (FORCE)`);
  await admin.query(`CREATE DATABASE ${DB}`);
  await admin.end();
}

export async function connectDb() {
  const c = new pg.Client({ connectionString: DB_URL });
  await c.connect();
  return c;
}

// Every port is below the range the system hands out for outgoing connections, or some other program's connection can be sitting on it.
export const OURS = { http: 26880 + OFFSET, tcp: 26881 + OFFSET, udp: 26882 + OFFSET, sip: 26960 + OFFSET, tls: 26961 + OFFSET, second: 26962 + OFFSET, rtpMin: 26100 + OFFSET, rtpMax: 26199 + OFFSET };
// Other servers some tests start beside the main one, with limits of their own.
export const SPARE = { http: 26890 + OFFSET, tcp: 26891 + OFFSET, udp: 26892 + OFFSET };

export function writeConfig(name, { sip = {}, port = OURS.sip, rtpMin = OURS.rtpMin, rtpMax = OURS.rtpMax, tls = false } = {}) {
  const lines = {
    udp_addr: `${HOST}:${port}`, tcp_addr: `${HOST}:${port}`, public_host: HOST, media_ip: HOST, rtp_port_min: rtpMin, rtp_port_max: rtpMax, lease_seconds: 4, drain_seconds: 3,
    invites_per_second: 1000, invite_burst: 1000, ban_seconds: -1, ...sip,
  };
  if (tls) Object.assign(lines, { tls_addr: `${HOST}:${OURS.tls}`, tls_cert_file: tls.cert, tls_key_file: tls.key, tls_ca_file: tls.cert });
  const path = join(tmp, name);
  writeFileSync(path, `port: ${OURS.http}
region: local
log_level: debug
rtc:
  udp_port: ${OURS.udp}
  tcp_port: ${OURS.tcp}
room:
  auto_create: true
  empty_timeout: 300
  departure_timeout: 20
control_plane:
  database_url: ${DB_URL}
  master_key: ${MASTER_KEY}
  public_ws_url: ws://${HOST}:${OURS.http}
  mail:
    dir: ${join(tmp, 'mail')}
media:
  ws_url: ws://${HOST}:${OURS.http}
  allow_private_addresses: true
  sip:
${Object.entries(lines).map(([k, v]) => `    ${k}: ${typeof v === 'string' ? JSON.stringify(v) : v}`).join('\n')}
`);
  return path;
}

// A signed-in user with a project and an API key, using the same public routes a person would.
export class Account {
  constructor(base) {
    this.base = base;
    this.cookie = '';
    this.project = '';
  }

  async req(method, path, body) {
    const h = { 'Content-Type': 'application/json', Cookie: this.cookie };
    if (this.project) h['X-Project-ID'] = this.project;
    const res = await fetch(`${this.base}/api/v1${path}`, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
    const setCookie = res.headers.get('set-cookie');
    if (setCookie) this.cookie = setCookie.split(';')[0];
    const text = await res.text();
    let json;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: res.status, body: json, text };
  }

  async ok(method, path, body, want = [200, 201, 204]) {
    const r = await this.req(method, path, body);
    assert.ok(want.includes(r.status), `${method} ${path}: HTTP ${r.status}: ${r.text}`);
    return r.body;
  }

  static async signUp(base, tag) {
    const a = new Account(base);
    const email = `${tag}-${Date.now()}-${randomBytes(3).toString('hex')}@example.com`;
    await a.ok('POST', '/auth/register', { email, password: 'a-good-long-password', name: tag, organization: `${tag} org` });
    const project = await a.ok('POST', '/projects', { name: `${tag} project` });
    a.project = project.id;
    const key = await a.ok('POST', '/keys', { name: 'tests' });
    a.apiKey = key.api_key;
    a.apiSecret = key.secret;
    return a;
  }
}

// Our stack.
export async function startOurs({ config, worker = true } = {}) {
  const path = config ?? writeConfig('config.yaml');
  await resetDatabase();
  const base = `http://${HOST}:${OURS.http}`;
  const server = startProcess('server', join(tmp, 'bin/streamkit-server'), ['--dev', '--config', path]);
  await waitFor('the server', async () => (await fetch(`${base}/`)).ok);
  const account = await Account.signUp(base, 'owner');
  const stack = makeStack('ours', base, `ws://${HOST}:${OURS.http}`, account.apiKey, account.apiSecret);
  Object.assign(stack, { server, account, sipPort: OURS.sip, configPath: path, workers: [] });
  stack.startWorker = async (name = 'sip-worker', cfg = path) => {
    // Its log says when it is up. A probe would not do: some of these workers are set up to answer nothing.
    const before = logOf(name).length;
    const w = startProcess(name, join(tmp, 'bin/streamkit-sip'), ['--dev', '--config', cfg]);
    stack.workers.push(w);
    await waitFor(`${name} to start`, () => {
      if (w.exitCode !== null) throw Object.assign(new Error(`${name} exited: ${logOf(name).slice(before)}`), { final: true });
      return logOf(name).slice(before).includes('sip worker started');
    });
    return w;
  };
  if (worker) await stack.startWorker();
  return stack;
}

const REF = { http: 26980 + OFFSET, tcp: 26981 + OFFSET, udp: 26982 + OFFSET, sip: 26970 + OFFSET, rtpMin: 26200 + OFFSET, rtpMax: 26299 + OFFSET };
const REF_KEY = 'devkey';
const REF_SECRET = 'a-reference-test-secret-that-is-long-enough';
// A database of its own in the shared Redis.
const REF_REDIS_DB = 11 + (OFFSET ? 1 : 0);

// The reference server and SIP bridge, to check that a test is about the protocol and not about our reading of it. The server is the
// released binary. The bridge is built by the harness from the read-only reference tree: see docs/deployment/sip.md, under Testing.
export function referenceAvailable() {
  if (!REFERENCE_SERVER || process.env.SIP_SKIP_REFERENCE) return false;
  try {
    return execFileSync('docker', ['images', '-q', ORACLE_IMAGE], { encoding: 'utf8' }).trim() !== '';
  } catch {
    return false;
  }
}

export async function startReference({ bridge = true } = {}) {
  // Its trunks live in Redis, in a database number of its own, and a run that failed may have left some behind.
  await run('docker', ['exec', 'elnova-streamkit-redis-1', 'redis-cli', '-n', String(REF_REDIS_DB), 'FLUSHDB']);
  const config = join(tmp, 'reference.yaml');
  writeFileSync(config, `port: ${REF.http}
bind_addresses: [${HOST}]
rtc:
  tcp_port: ${REF.tcp}
  udp_port: ${REF.udp}
  node_ip: ${HOST}
  use_external_ip: false
  enable_loopback_candidate: true
redis:
  address: 127.0.0.1:6380
  db: ${REF_REDIS_DB}
keys:
  ${REF_KEY}: ${REF_SECRET}
logging:
  level: info
`);
  const server = startProcess('reference', REFERENCE_SERVER, ['--config', config]);
  const base = `http://${HOST}:${REF.http}`;
  await waitFor('the reference server', async () => (await fetch(`${base}/`)).ok, { timeout: 30000 });
  const stack = makeStack('reference', base, `ws://${HOST}:${REF.http}`, REF_KEY, REF_SECRET);
  Object.assign(stack, { server, sipPort: REF.sip, reference: true });
  if (bridge) {
    const body = `api_key: ${REF_KEY}
api_secret: ${REF_SECRET}
ws_url: ws://${HOST}:${REF.http}
redis:
  address: 127.0.0.1:6380
  db: ${REF_REDIS_DB}
sip_port: ${REF.sip}
sip_port_listen: ${REF.sip}
listen_ip: ${HOST}
local_net: 127.0.0.0/8
nat_1_to_1_ip: ${HOST}
rtp_port: ${REF.rtpMin}-${REF.rtpMax}
use_external_ip: false
logging:
  level: info
`;
    stack.containerName = `elnova-sip-oracle-${process.pid}`;
    stack.bridge = startProcess('reference-bridge', 'docker', ['run', '--rm', '--name', stack.containerName, '--network', 'host', '-m', '2g', '-e', `SIP_CONFIG_BODY=${body}`, ORACLE_IMAGE]);
    await waitForSipPort(REF.sip, 40000);
  }
  return stack;
}

export async function stopReference(stack) {
  if (stack.containerName) {
    await run('docker', ['stop', '-t', '3', stack.containerName]).catch(() => {});
    if (stack.bridge) await stack.bridge.exited;
  }
  await stopProcess(stack.server);
}

function makeStack(name, base, ws, apiKey, apiSecret) {
  return {
    name, base, ws, apiKey, apiSecret,
    sip: new SipClient(base, apiKey, apiSecret),
    rooms: new RoomServiceClient(base, apiKey, apiSecret),
    async token(identity, room, grants = {}) {
      const at = new AccessToken(apiKey, apiSecret, { identity, name: identity });
      at.addGrant({ roomJoin: true, room, canPublish: true, canSubscribe: true, canPublishData: true, ...grants });
      return at.toJwt();
    },
    // Calls an API method the official client has no wrapper for, the way it would.
    async twirp(method, body, { sip = { admin: true, call: true }, video = {} } = {}) {
      const at = new AccessToken(apiKey, apiSecret, { identity: 'tester' });
      at.addSIPGrant(sip);
      at.addGrant(video);
      const res = await fetch(`${base}/twirp/livekit.SIP/${method}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${await at.toJwt()}` }, body: JSON.stringify(body),
      });
      const text = await res.text();
      let json;
      try {
        json = JSON.parse(text);
      } catch {}
      return { status: res.status, body: json, text };
    },
  };
}

export async function waitForSipPort(port, timeout = 20000) {
  await waitFor(`SIP on port ${port}`, async () => {
    const r = await rawExchange(port, options(port), 400);
    return r.length > 0;
  }, { timeout, every: 200 });
}

// --- A raw SIP socket. It knows the protocol's text and nothing else, and shares no code with what is being tested. ---

export const newTag = () => randomBytes(6).toString('hex');

export function options(port, { userAgent = 'tests' } = {}) {
  const id = newTag();
  return `OPTIONS sip:${HOST}:${port} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:9;branch=z9hG4bK${id}\r\nFrom: <sip:probe@${HOST}>;tag=${id}\r\nTo: <sip:${HOST}:${port}>\r\nCall-ID: ${id}@tests\r\nCSeq: 1 OPTIONS\r\nMax-Forwards: 70\r\nUser-Agent: ${userAgent}\r\nContent-Length: 0\r\n\r\n`;
}

export const SDP = (port, codecs = '0 101', extra = '') => `v=0\r\no=raw 1 1 IN IP4 ${HOST}\r\ns=raw\r\nc=IN IP4 ${HOST}\r\nt=0 0\r\nm=audio ${port} RTP/AVP ${codecs}\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n${extra}`;

export function invite(port, { from = '+15550100', to = '+15550199', callId = `${newTag()}@tests`, tag = newTag(), branch = newTag(), cseq = 1, sdp = SDP(4000), userAgent = 'tests', headers = '', localPort = 9, host = HOST, method = 'INVITE', toTag = '', uri } = {}) {
  const body = sdp ?? '';
  const requestUri = uri ?? `sip:${to}@${host}:${port}`;
  return `${method} ${requestUri} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:${localPort};rport;branch=z9hG4bK${branch}\r\nFrom: <sip:${from}@${HOST}>;tag=${tag}\r\nTo: <sip:${to}@${host}>${toTag ? `;tag=${toTag}` : ''}\r\nCall-ID: ${callId}\r\nCSeq: ${cseq} ${method}\r\nContact: <sip:${from}@${HOST}:${localPort}>\r\nMax-Forwards: 70\r\nUser-Agent: ${userAgent}\r\n${headers}${body ? 'Content-Type: application/sdp\r\n' : ''}Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`;
}

export function parseSip(text) {
  const [head, ...rest] = text.split('\r\n\r\n');
  const lines = head.split('\r\n');
  const first = lines[0];
  const headers = {};
  for (const l of lines.slice(1)) {
    const i = l.indexOf(':');
    if (i > 0) (headers[l.slice(0, i).trim().toLowerCase()] ??= []).push(l.slice(i + 1).trim());
  }
  const m = /^SIP\/2\.0 (\d+) ?(.*)$/.exec(first);
  return { first, status: m ? Number(m[1]) : 0, reason: m ? m[2] : '', headers, body: rest.join('\r\n\r\n'), header: (n) => headers[n.toLowerCase()]?.[0] ?? '', text };
}

// A UDP socket that sends SIP text and keeps what comes back.
export class RawPeer {
  constructor() {
    this.messages = [];
    this.socket = createSocket('udp4');
    this.socket.on('message', (m) => this.messages.push(parseSip(m.toString())));
  }

  async open() {
    await new Promise((r) => this.socket.bind(0, HOST, r));
    this.port = this.socket.address().port;
    return this;
  }

  send(port, text) {
    return new Promise((resolve, reject) => this.socket.send(text, port, HOST, (e) => (e ? reject(e) : resolve())));
  }

  async waitForStatus(status, timeout = 5000) {
    return waitFor(`a ${status}`, () => this.messages.find((m) => m.status === status), { timeout, every: 20 });
  }

  // Waits for a final response.
  async final(timeout = 5000) {
    return waitFor('a final response', () => this.messages.find((m) => m.status >= 200), { timeout, every: 20 });
  }

  close() {
    this.socket.close();
  }
}

// Answers a raw request, as a very small user agent would.
export function respondRaw(peer, msg, status, reason, extra = '', body = '') {
  const via = msg.headers.via.map((v) => `Via: ${v}\r\n`).join('');
  const to = msg.header('to');
  const tagged = /;tag=/.test(to) ? to : `${to};tag=raw${msg.header('call-id').slice(0, 8)}`;
  const text = `SIP/2.0 ${status} ${reason}\r\n${via}From: ${msg.header('from')}\r\nTo: ${tagged}\r\nCall-ID: ${msg.header('call-id')}\r\nCSeq: ${msg.header('cseq')}\r\n${extra}${body ? 'Content-Type: application/sdp\r\n' : ''}Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`;
  const m = /rport=(\d+)/.exec(msg.headers.via[0]);
  const port = m ? Number(m[1]) : Number(/:(\d+)/.exec(msg.headers.via[0].split(';')[0])[1]);
  return peer.send(port, text);
}

export async function rawExchange(port, text, timeout = 1000) {
  const peer = await new RawPeer().open();
  try {
    await peer.send(port, text.replaceAll(`${HOST}:9;`, `${HOST}:${peer.port};`));
    await sleep(timeout);
    return peer.messages;
  } finally {
    peer.close();
  }
}

// Answers a digest challenge, from RFC 3261 and RFC 2617. Written here rather than shared with anything being tested.
export function digestHeader(challenge, { username, password, method = 'INVITE', uri }) {
  const get = (name) => new RegExp(`${name}="?([^",]+)"?`).exec(challenge)?.[1] ?? '';
  const md5 = (s) => createHash('md5').update(s).digest('hex');
  const realm = get('realm');
  const nonce = get('nonce');
  const cnonce = newTag();
  const nc = '00000001';
  const ha1 = md5(`${username}:${realm}:${password}`);
  const ha2 = md5(`${method}:${uri}`);
  const response = md5(`${ha1}:${nonce}:${nc}:${cnonce}:auth:${ha2}`);
  return `Digest username="${username}", realm="${realm}", nonce="${nonce}", uri="${uri}", response="${response}", algorithm=MD5, qop=auth, nc=${nc}, cnonce="${cnonce}"`;
}

// --- pjsua: an independent SIP stack and media engine. ---

let nextPhonePort = 27000 + OFFSET;

// Writes a WAV file of a tone, for a phone to play.
export function toneFile(name, freq, seconds = 30, rate = 8000) {
  const path = join(tmp, name);
  const n = rate * seconds;
  const data = Buffer.alloc(n * 2);
  for (let i = 0; i < n; i++) data.writeInt16LE(Math.round(10000 * Math.sin((2 * Math.PI * freq * i) / rate)), i * 2);
  const h = Buffer.alloc(44);
  h.write('RIFF', 0);
  h.writeUInt32LE(36 + data.length, 4);
  h.write('WAVEfmt ', 8);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(1, 22);
  h.writeUInt32LE(rate, 24);
  h.writeUInt32LE(rate * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write('data', 36);
  h.writeUInt32LE(data.length, 40);
  writeFileSync(path, Buffer.concat([h, data]));
  return path;
}

export function readWav(path) {
  const b = readFileSync(path);
  let off = 12;
  let rate = 8000;
  let data = Buffer.alloc(0);
  while (off + 8 <= b.length) {
    const id = b.toString('ascii', off, off + 4);
    let size = b.readUInt32LE(off + 4);
    if (id === 'fmt ') rate = b.readUInt32LE(off + 12);
    if (id === 'data') {
      size = Math.min(size, b.length - off - 8);
      data = b.subarray(off + 8, off + 8 + size);
      break;
    }
    off += 8 + size + (size % 2);
  }
  const samples = new Int16Array(data.length >> 1);
  for (let i = 0; i < samples.length; i++) samples[i] = data.readInt16LE(i * 2);
  return { rate, samples };
}

export function goertzel(samples, freq, rate) {
  let c = 0;
  let s = 0;
  for (let i = 0; i < samples.length; i++) {
    const a = (2 * Math.PI * freq * i) / rate;
    c += samples[i] * Math.cos(a);
    s += samples[i] * Math.sin(a);
  }
  return (2 * Math.hypot(c, s)) / Math.max(1, samples.length);
}

// Checks a recording holds a tone: strong at that frequency, and well above the others that were on offer.
export function assertToneInWav(path, freq, others = []) {
  const { samples, rate } = readWav(path);
  assert.ok(samples.length > rate, `the recording is only ${samples.length} samples long`);
  // The last part, because the start holds setup and ringing. A recording may end in silence, so skip the very end too.
  const part = samples.subarray(Math.floor(samples.length * 0.4), Math.floor(samples.length * 0.95));
  const level = goertzel(part, freq, rate);
  assert.ok(level > 200, `no tone at ${freq} Hz in ${path}: level ${level.toFixed(1)}`);
  for (const o of others) {
    const other = goertzel(part, o, rate);
    assert.ok(level > other * 5, `${o} Hz is at ${other.toFixed(1)}, against ${level.toFixed(1)} at ${freq} Hz`);
  }
  return level;
}

export class Phone {
  constructor(name, { args = [], play, record, autoAnswer, user = '+15550100', realm, username, password, transport = 'udp', tlsCert, srtp, codecs } = {}) {
    if (!PJSUA) throw new Error('pjsua is not built: see docs/deployment/sip.md, under Testing');
    this.name = name;
    this.sipPort = nextPhonePort;
    this.rtpPort = nextPhonePort + 2;
    nextPhonePort += 20;
    this.recordPath = record ? join(tmp, record) : null;
    if (this.recordPath) rmSync(this.recordPath, { force: true });
    const a = ['--null-audio', `--local-port=${this.sipPort}`, `--ip-addr=${HOST}`, `--bound-addr=${HOST}`, `--rtp-port=${this.rtpPort}`, '--no-vad', '--ec-tail=0', '--max-calls=4',
      '--clock-rate=8000', '--snd-clock-rate=8000', '--log-level=4', '--app-log-level=4', '--no-color', `--id=sip:${user}@${HOST}`, '--no-tones'];
    if (transport !== 'tcp' && transport !== 'tls') a.push('--no-tcp');
    if (transport === 'tcp') a.push('--no-udp');
    if (transport === 'tls') a.push('--use-tls', '--no-udp', '--no-tcp', `--tls-ca-file=${tlsCert.cert}`, `--tls-cert-file=${tlsCert.cert}`, `--tls-privkey-file=${tlsCert.key}`, '--tls-verify-server');
    if (play) a.push(`--play-file=${play}`, '--auto-play');
    if (record) a.push(`--rec-file=${this.recordPath}`, '--auto-rec');
    if (autoAnswer) a.push(`--auto-answer=${autoAnswer}`);
    if (username) a.push(`--realm=${realm ?? '*'}`, `--username=${username}`, `--password=${password}`);
    if (srtp !== undefined) a.push(`--use-srtp=${srtp}`, '--srtp-secure=0');
    if (codecs) {
      a.push('--dis-codec=*');
      for (const c of codecs) a.push(`--add-codec=${c}`);
    }
    a.push(...args);
    this.log = '';
    // Its output is a pipe here, so unbuffering it is the only way to see what it did when it did it.
    this.child = spawn('stdbuf', ['-o0', '-e0', PJSUA, ...a], { cwd: tmp, stdio: ['pipe', 'pipe', 'pipe'] });
    const out = createWriteStream(join(tmp, `phone-${name}.log`), { flags: 'a' });
    const onData = (d) => { this.log += d.toString(); out.write(d); };
    this.child.stdout.on('data', onData);
    this.child.stderr.on('data', onData);
    this.exited = new Promise((resolve) => this.child.on('exit', (code, signal) => { children.delete(this.child); resolve({ code, signal }); }));
    children.add(this.child);
    this.logMark = 0;
  }

  send(line) {
    this.child.stdin.write(`${line}\n`);
  }

  // Everything logged since this was last asked, so a wait does not match what happened to an earlier call.
  mark() {
    this.logMark = this.log.length;
  }

  async waitForLog(re, timeout = 15000) {
    return waitFor(`${this.name} to log ${re}`, () => re.exec(this.log.slice(this.logMark)), { timeout, every: 30 });
  }

  async ready() {
    await this.waitForLog(/PJSUA|pjsua version|Ready|\[\s*Call/i, 10000).catch(() => {});
    await sleep(400);
  }

  call(uri) {
    this.send('m');
    this.send(uri);
  }

  async confirmed(timeout = 20000) {
    return this.waitForLog(/state changed to CONFIRMED/, timeout);
  }

  async disconnected(timeout = 20000) {
    return this.waitForLog(/is DISCONNECTED \[reason=(\d+)/, timeout);
  }

  hangup() {
    this.send('h');
  }

  dtmf(digits) {
    this.send('#');
    this.send(digits);
  }

  sipInfoDtmf(digit) {
    this.send('*');
    this.send(digit);
  }

  hold() {
    this.send('H');
  }

  reinvite() {
    this.send('v');
  }

  async digitsHeard() {
    return [...this.log.slice(this.logMark).matchAll(/Incoming DTMF on call \d+: (.)/g)].map((m) => m[1]).join('');
  }

  async stop() {
    if (!children.has(this.child)) return;
    this.send('q');
    const done = await Promise.race([this.exited, sleep(4000).then(() => null)]);
    if (!done) {
      this.child.kill('SIGKILL');
      await this.exited;
    }
  }
}

// --- Real Chrome, with the official client library. ---

let browserState;

async function startPages() {
  await build({
    entryPoints: [join(here, 'pages/client.ts')],
    outfile: join(tmp, 'client.js'),
    bundle: true, format: 'esm', target: 'es2021', logLevel: 'warning', nodePaths: [join(here, 'node_modules')],
  });
  const js = readFileSync(join(tmp, 'client.js'));
  const server = createServer((req, res) => {
    if (req.url === '/client.js') {
      res.setHeader('content-type', 'text/javascript');
      res.end(js);
      return;
    }
    res.setHeader('content-type', 'text/html');
    res.end('<!doctype html><title>sip test</title><script type="module" src="/client.js"></script><body></body>');
  });
  await new Promise((r) => server.listen(0, HOST, r));
  return server;
}

export async function startBrowser() {
  const pages = await startPages();
  const browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  browserState = { pages, browser, base: `http://${HOST}:${pages.address().port}` };
  return browserState;
}

export async function stopBrowser() {
  await browserState?.browser.close();
  browserState?.pages.close();
  browserState = undefined;
}

// Chrome only starts if a test needs it, so tests with nobody in a room stay light.
export async function person(stack, identity, room, { tone } = {}) {
  browserState ??= await startBrowser();
  const ctx = await browserState.browser.newContext({ permissions: ['microphone'] });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(browserState.base);
  await page.waitForSelector('body[data-ready="1"]', { state: 'attached', timeout: 20000 });
  const token = await stack.token(identity, room);
  await page.evaluate(([ws, t]) => window.harness.join(ws, t), [stack.ws, token]);
  if (tone) await page.evaluate((f) => window.harness.publishTone(f), tone);
  const p = {
    identity, page, errors,
    participants: () => page.evaluate(() => window.harness.participants()),
    levels: (who, freqs, seconds = 2) => page.evaluate(([w, f, s]) => window.harness.levels(w, f, s), [who, freqs, seconds]),
    digits: () => page.evaluate(() => window.harness.digits()),
    left: () => page.evaluate(() => window.harness.left()),
    sendDigit: (code, digit) => page.evaluate(([c, d]) => window.harness.sendDigit(c, d), [code, digit]),
    rpc: (to, method, payload = '') => page.evaluate(([w, m, b]) => window.harness.rpc(w, m, b), [to, method, payload]),
    close: async () => { await page.evaluate(() => window.harness.leave()).catch(() => {}); await ctx.close(); },
  };
  return p;
}

// Waits until someone in the room has decoded a tone from a participant, and checks it stands well clear of the other frequency.
export async function assertHears(p, from, freq, other, timeout = 20000) {
  let last;
  await waitFor(`${p.identity} to hear ${freq} Hz from ${from}`, async () => {
    last = await p.levels(from, [freq, other], 2);
    return last.samples > 48000 && last.levels[freq] > 0.02 && last.levels[freq] > last.levels[other] * 5;
  }, { timeout, every: 500 }).catch((e) => { throw new Error(`${e.message}: last measured ${JSON.stringify(last)}`); });
  return last;
}

export async function findParticipant(p, predicate, timeout = 20000) {
  return waitFor(`${p.identity} to see a participant`, async () => (await p.participants()).find(predicate), { timeout });
}

export function prepareDir() {
  mkdirSync(tmp, { recursive: true });
  mkdirSync(join(tmp, 'mail'), { recursive: true });
  execFileSync('sh', ['-c', `rm -f '${tmp}'/*.log '${tmp}'/*.wav`]);
}

// A self-signed certificate for loopback.
export function makeCertificate(name = 'sip', ip = HOST) {
  const cert = join(tmp, `${name}-cert.pem`);
  const key = join(tmp, `${name}-key.pem`);
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '2', '-subj', `/CN=${ip}`, '-addext', `subjectAltName=IP:${ip}`], { stdio: 'ignore' });
  return { cert, key };
}

export async function stopAll() {
  await Promise.all([...children].map(async (c) => {
    c.kill('SIGKILL');
    await new Promise((r) => { c.once('exit', r); setTimeout(r, 3000); });
  }));
}

export const sipUri = (stack, user = '+15550199', transport) => `sip:${user}@${HOST}:${stack.sipPort}${transport ? `;transport=${transport}` : ''}`;
