// Starts the real stack for the media tests, and holds the checks they share: a server, database, workers, ffmpeg and
// Chrome. It keeps to ports 57880-57882 for the server and 57935 for RTMP, and only ever stops processes it started.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createWriteStream, existsSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { connect } from 'node:net';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { build } from 'esbuild';
import pg from 'pg';
import { chromium } from 'playwright-core';

const run = promisify(execFile);
export const here = dirname(fileURLToPath(import.meta.url));
export const repo = join(here, '../..');
export const tmp = join(here, '.tmp');
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export const HTTP_PORT = 57880;
export const RTMP_PORT = 57935;
export const BASE = `http://127.0.0.1:${HTTP_PORT}`;
export const WS = `ws://127.0.0.1:${HTTP_PORT}`;
const PG_ADMIN = 'postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit';
const MASTER_KEY = '6d656469612d746573747320617265206e6f74206120736563726574206b657921'.slice(0, 64);
// MEDIA_DB keeps runs from different checkouts out of each other's way.
const DB = process.env.MEDIA_DB ?? 'streamkit_media';
const DB_URL = `postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/${DB}?sslmode=disable`;
// Workers connect as the role made for them, as production does, so any query they need and may not run fails a test here.
const WORKER_DB_PASSWORD = 'media-tests-worker-password';
export const WORKER_DB_URL = `postgres://streamkit_worker:${WORKER_DB_PASSWORD}@127.0.0.1:5433/${DB}?sslmode=disable`;
const workerEnv = process.env.MEDIA_OWNER_WORKERS ? {} : { STREAMKIT_DATABASE_URL: WORKER_DB_URL };

export async function buildBinaries() {
  const env = { ...process.env, PATH: `${join(repo, '../.toolchain/go/bin')}:${process.env.PATH}`, GOPATH: join(repo, '../.toolchain/gopath'), GOTMPDIR: process.env.GOTMPDIR ?? join(repo, '../.toolchain/tmp'), GOTOOLCHAIN: 'local' };
  const cwd = repo;
  // MEDIA_RACE=1 runs the whole stack under Go's race detector. Any race it finds ends up in the process logs.
  const flags = process.env.MEDIA_RACE ? ['-race'] : [];
  await run('go', ['build', ...flags, '-o', 'bin/streamkit-server-media', './server/cmd/streamkit-server'], { cwd, env });
  await run('go', ['build', ...flags, '-o', 'bin/streamkit-egress', './egress/cmd/streamkit-egress'], { cwd, env });
  await run('go', ['build', ...flags, '-o', 'bin/streamkit-ingress', './ingress/cmd/streamkit-ingress'], { cwd, env });
}

async function resetDatabase() {
  const admin = new pg.Client({ connectionString: PG_ADMIN });
  await admin.connect();
  await admin.query(`DROP DATABASE IF EXISTS ${DB} WITH (FORCE)`);
  await admin.query(`CREATE DATABASE ${DB}`);
  await admin.end();
}

export function writeConfig(name, extra = '', { lease = process.env.MEDIA_LEASE ?? 6, stall = 60, allowPrivate = true, egress = '' } = {}) {
  const path = join(tmp, name);
  writeFileSync(path, `port: ${HTTP_PORT}
region: local
log_level: debug
rtc:
  udp_port: 57882
  tcp_port: 57881
room:
  auto_create: true
  empty_timeout: 300
  departure_timeout: 20
control_plane:
  database_url: ${DB_URL}
  master_key: ${MASTER_KEY}
  public_dashboard_url: http://localhost:3000
  public_ws_url: ${WS}
  allow_private_webhooks: true
  mail:
    dir: ${join(tmp, 'mail')}
media:
  ws_url: ${WS}
  allow_private_addresses: ${allowPrivate}
  storage:
    kind: local
    dir: ${join(tmp, 'recordings')}
  egress:
    work_dir: ${join(tmp, 'work')}
    lease_seconds: ${lease}
${egress}
  ingress:
    rtmp_addr: 127.0.0.1:${RTMP_PORT}
    lease_seconds: ${lease}
    stall_seconds: ${stall}
${extra}`);
  return path;
}

const children = new Set();

// Starts a process and keeps its output in a log file.
export function startProcess(name, binary, args, { env } = {}) {
  const log = createWriteStream(join(tmp, `${name}.log`), { flags: 'a' });
  const child = spawn(binary, args, { cwd: repo, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, ...env } });
  child.stdout.pipe(log);
  child.stderr.pipe(log);
  child.name = name;
  child.exited = new Promise((resolve) => child.on('exit', (code, signal) => { children.delete(child); resolve({ code, signal }); }));
  children.add(child);
  return child;
}

export async function stopProcess(child, signal = 'SIGTERM', timeoutMs = 45000) {
  if (!children.has(child)) return;
  child.kill(signal);
  const done = await Promise.race([child.exited, sleep(timeoutMs).then(() => null)]);
  if (!done) {
    child.kill('SIGKILL');
    await child.exited;
  }
}

export async function stopAll() {
  await Promise.all([...children].map((c) => stopProcess(c)));
}

async function waitFor(what, fn, { timeout = 20000, every = 200 } = {}) {
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
}
export { waitFor };

export async function startServer(config) {
  const server = startProcess('server', join(repo, 'bin/streamkit-server-media'), ['--dev', '--config', config], { env: { STREAMKIT_WORKER_DB_PASSWORD: WORKER_DB_PASSWORD } });
  await waitFor('the server', async () => (await fetch(`${BASE}/`)).ok);
  return server;
}

export function startEgressWorker(config, args = [], opts = {}) {
  return startProcess(opts.name ?? 'egress', join(repo, 'bin/streamkit-egress'), ['--dev', '--config', config, ...args], { ...opts, env: { ...workerEnv, ...opts.env } });
}

export function startIngressWorker(config, args = [], opts = {}) {
  return startProcess(opts.name ?? 'ingress', join(repo, 'bin/streamkit-ingress'), ['--dev', '--config', config, ...args], { ...opts, env: { ...workerEnv, ...opts.env } });
}

// A signed-in user with a project and an API key.
export class Api {
  constructor(base = BASE) {
    this.base = base;
    this.cookie = '';
    this.project = '';
  }

  async req(method, path, body, headers = {}) {
    const h = { 'Content-Type': 'application/json', Cookie: this.cookie, ...headers };
    if (this.project) h['X-Project-ID'] = this.project;
    const res = await fetch(`${this.base}/api/v1${path}`, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
    const setCookie = res.headers.get('set-cookie');
    if (setCookie) this.cookie = setCookie.split(';')[0];
    const text = await res.text();
    let json;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: res.status, body: json, text, headers: res.headers };
  }

  async ok(method, path, body, want = [200, 201, 204]) {
    const r = await this.req(method, path, body);
    assert.ok(want.includes(r.status), `${method} ${path}: HTTP ${r.status}: ${r.text}`);
    return r.body;
  }

  static async signUp(tag, base = BASE) {
    const a = new Api(base);
    const email = `${tag}-${Date.now()}@example.com`;
    await a.ok('POST', '/auth/register', { email, password: 'a-good-long-password', name: tag, organization: `${tag} org` });
    const project = await a.ok('POST', '/projects', { name: `${tag} project` });
    a.project = project.id;
    const key = await a.ok('POST', '/keys', { name: 'tests' });
    a.apiKey = key.api_key;
    a.apiSecret = key.secret;
    return a;
  }
}

export async function connectDb() {
  const c = new pg.Client({ connectionString: DB_URL });
  await c.connect();
  return c;
}

// Serves the participant page.
async function startPages() {
  await build({
    entryPoints: [join(here, 'pages/client.ts')],
    outfile: join(here, 'pages/client.js'),
    bundle: true,
    format: 'esm',
    target: 'es2021',
    logLevel: 'warning',
    alias: { '@elnova-streamkit/client': join(repo, 'sdk-js/src/index.ts') },
    nodePaths: [join(repo, 'sdk-js/node_modules')],
  });
  const types = { '.html': 'text/html', '.js': 'text/javascript' };
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    const name = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
    if (!['index.html', 'client.js'].includes(name)) {
      res.statusCode = 404;
      res.end();
      return;
    }
    res.setHeader('content-type', types[name.slice(name.lastIndexOf('.'))]);
    res.end(await readFile(join(here, 'pages', name)));
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  return server;
}

export async function startBrowser({ args = [] } = {}) {
  const pages = await startPages();
  const browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required', ...args],
  });
  return { browser, pages, base: `http://127.0.0.1:${pages.address().port}` };
}

// Boots everything once for a test file.
export async function setUp({ config = writeConfig('config.yaml'), egress = true, ingress = true, browser = true } = {}) {
  mkdirSync(tmp, { recursive: true });
  const stack = { config };
  await resetDatabase();
  stack.server = await startServer(config);
  if (egress) stack.egress = startEgressWorker(config);
  if (ingress) stack.ingress = startIngressWorker(config);
  if (browser) stack.chrome = await startBrowser();
  stack.api = await Api.signUp('owner');
  if (egress) await waitForWorker(stack.api, 'egress_worker');
  if (ingress) await waitForWorker(stack.api, 'ingress_worker');
  return stack;
}

export async function tearDown(stack) {
  await stack.chrome?.browser.close();
  stack.chrome?.pages.close();
  await stopAll();
}

export function prepareTmp() {
  mkdirSync(tmp, { recursive: true });
  for (const f of ['recordings', 'work', 'mail']) rmSync(join(tmp, f), { recursive: true, force: true });
  // Each test file starts clean, but what the last one logged is what explains a failure that only shows up now and then.
  const kept = join(tmp, 'old-logs', `${Date.now()}`);
  for (const f of readdirSync(tmp).filter((name) => name.endsWith('.log'))) {
    mkdirSync(kept, { recursive: true });
    renameSync(join(tmp, f), join(kept, f));
  }
}

// A worker's row stays fresh for a while after it dies, so after a restart, this is what says the new one is up.
export async function waitForPort(port, timeout = 20000) {
  await waitFor(`port ${port}`, () => new Promise((resolve) => {
    const socket = connect(port, '127.0.0.1');
    socket.on('connect', () => { socket.destroy(); resolve(true); });
    socket.on('error', () => resolve(false));
  }), { timeout });
}

export async function waitForWorker(api, kind, timeout = 20000) {
  await waitFor(`a ${kind}`, async () => {
    const r = await api.ok('GET', '/servers');
    return r.items.some((s) => s.stats.kind === kind && s.stats.max_jobs > 0 && Date.now() - Date.parse(s.last_seen_at) < 15000);
  }, { timeout });
}

// Someone in a call, in real Chrome with fake devices.
export async function joinRoom(stack, api, identity, room, { publish = true, autosub = true, grants = {}, ws = WS } = {}) {
  const token = (await api.ok('POST', '/tokens', { identity, room, name: identity, ...grants })).token;
  const ctx = await stack.chrome.browser.newContext({ permissions: ['camera', 'microphone'], viewport: { width: 1280, height: 720 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const q = new URLSearchParams({ ws, token, publish: publish ? '1' : '0', autosub: autosub ? '1' : '0' });
  await page.goto(`${stack.chrome.base}/?${q}`);
  await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
  const state = await page.$eval('#state', (e) => `${e.dataset.state} ${e.textContent}`);
  assert.ok(state.startsWith('connected'), `${identity} could not connect: ${state}`);
  return { page, ctx, identity, errors, close: () => ctx.close() };
}

export const received = (c) => c.page.evaluate(() => window.harness.received());

export async function waitForTracks(c, identity, kinds, timeout = 30000) {
  await waitFor(`${c.identity} to receive ${kinds} of ${identity}`, async () => {
    const rows = await received(c);
    return kinds.every((k) => rows.some((r) => r.identity === identity && r.kind === k));
  }, { timeout });
}

// Proves moving video is being decoded, using the same evidence as the conformance suite: the decoder advances, and the
// pixels change.
export async function assertVideoFlows(c, identity, { minFrames = 15, windowMs = 2500 } = {}) {
  const find = async () => (await received(c)).find((r) => r.identity === identity && r.kind === 'video');
  const first = await waitFor(`first decoded frame of ${identity}`, async () => {
    const row = await find();
    if (!row) return null;
    const hash = await c.page.evaluate((sid) => window.harness.frameHash(sid), row.sid);
    return hash ? { row, hash } : null;
  }, { timeout: 30000 });
  await sleep(windowMs);
  const after = await find();
  const hash2 = await c.page.evaluate((sid) => window.harness.frameHash(sid), after.sid);
  const frames = after.framesDecoded - first.row.framesDecoded;
  assert.ok(frames >= minFrames, `decoded only ${frames} frames of ${identity} in ${windowMs} ms`);
  assert.ok(after.bytesReceived > first.row.bytesReceived, 'no bytes received');
  assert.ok(hash2.width > 0 && hash2.height > 0, 'frame has no size');
  assert.notEqual(first.hash.hash, hash2.hash, `pixels of ${identity} did not change`);
  assert.ok(hash2.max - hash2.min > 16, `the frame of ${identity} is one flat colour: ${JSON.stringify(hash2)}`);
  return { ...after, frame: hash2, frames };
}

// Proves audio is arriving, and is not silence.
export async function assertAudioFlows(c, identity, { windowMs = 2000 } = {}) {
  const find = async () => (await received(c)).find((r) => r.identity === identity && r.kind === 'audio');
  const a = await waitFor(`audio of ${identity}`, find, { timeout: 30000 });
  await sleep(windowMs);
  const b = await find();
  const samples = b.totalSamplesReceived - a.totalSamplesReceived;
  assert.ok(b.packetsReceived - a.packetsReceived > 20, `audio packets of ${identity} are not arriving`);
  assert.ok(samples > 24000, `only ${samples} audio samples of ${identity} in ${windowMs} ms`);
  assert.ok(b.totalAudioEnergy > a.totalAudioEnergy, `the audio of ${identity} is silent`);
  return b;
}

export async function ffprobe(path) {
  const { stdout } = await run('ffprobe', ['-v', 'error', '-print_format', 'json', '-show_format', '-show_streams', path], { maxBuffer: 1 << 26 });
  const info = JSON.parse(stdout);
  return {
    format: info.format,
    video: info.streams.find((s) => s.codec_type === 'video'),
    audio: info.streams.find((s) => s.codec_type === 'audio'),
    duration: Number(info.format.duration),
  };
}

// Decodes every frame of a region and reports on its pixels. YDIF is how much each frame differs from the one before.
export async function videoStats(path, crop = '') {
  const filter = `${crop ? `${crop},` : ''}signalstats,metadata=print:file=-`;
  const { stdout } = await run('ffmpeg', ['-hide_banner', '-v', 'error', '-i', path, '-an', '-vf', filter, '-f', 'null', '-'], { maxBuffer: 1 << 28 });
  const rows = { YMIN: [], YMAX: [], YAVG: [], YDIF: [], SATAVG: [] };
  for (const line of stdout.split('\n')) {
    const m = line.match(/lavfi\.signalstats\.(YMIN|YMAX|YAVG|YDIF|SATAVG)=([\d.]+)/);
    if (m) rows[m[1]].push(Number(m[2]));
  }
  const mean = (a) => a.reduce((x, y) => x + y, 0) / (a.length || 1);
  return {
    frames: rows.YAVG.length,
    contrast: mean(rows.YMAX) - mean(rows.YMIN),
    yavg: mean(rows.YAVG),
    ydif: mean(rows.YDIF),
    sat: mean(rows.SATAVG),
  };
}

// Measures loudness by decoding all the audio.
export async function audioStats(path) {
  const { stderr } = await run('ffmpeg', ['-hide_banner', '-i', path, '-vn', '-af', 'volumedetect', '-f', 'null', '-'], { maxBuffer: 1 << 26 });
  const mean = stderr.match(/mean_volume: (-?[\d.]+) dB/);
  const peak = stderr.match(/max_volume: (-?[\d.]+) dB/);
  return { mean: mean ? Number(mean[1]) : -Infinity, peak: peak ? Number(peak[1]) : -Infinity };
}

export async function waitForStatus(api, id, statuses, timeout = 60000) {
  const wanted = [statuses].flat();
  return waitFor(`egress ${id} to reach ${wanted}`, async () => {
    const job = await api.ok('GET', `/egress/${id}`);
    if (wanted.includes(job.status)) return job;
    if (['complete', 'failed', 'aborted'].includes(job.status)) {
      const e = new Error(`egress ${id} ended as ${job.status} instead of ${wanted}: ${job.error}`);
      e.final = true;
      throw e;
    }
    return null;
  }, { timeout, every: 250 });
}

// Saves a recording through the API, the way a browser would download it.
export async function download(api, id, name = `${id}.bin`) {
  const res = await fetch(`${BASE}/api/v1/recordings/${id}/file`, { headers: { Cookie: api.cookie, 'X-Project-ID': api.project } });
  assert.equal(res.status, 200, `downloading ${id}: HTTP ${res.status}`);
  const path = join(tmp, name);
  writeFileSync(path, Buffer.from(await res.arrayBuffer()));
  return path;
}

// The memory of everything below a process. PSS shares out what processes have in common, which matters for Chrome.
export function memoryBelowMB(root) {
  const children = new Map();
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = readFileSync(`/proc/${name}/stat`, 'latin1');
      const ppid = Number(stat.slice(stat.lastIndexOf(')') + 2).split(' ')[1]);
      if (!children.has(ppid)) children.set(ppid, []);
      children.get(ppid).push(Number(name));
    } catch {}
  }
  const pending = [...(children.get(root) ?? [])];
  let kb = 0;
  while (pending.length) {
    const pid = pending.pop();
    pending.push(...(children.get(pid) ?? []));
    try {
      kb += Number(/^Pss:\s+(\d+) kB/m.exec(readFileSync(`/proc/${pid}/smaps_rollup`, 'latin1'))?.[1] ?? 0);
    } catch {}
  }
  return kb / 1024;
}

// Processes below this one that have exited with nothing waiting for them.
export function zombiesBelow(root) {
  const out = [];
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = readFileSync(`/proc/${name}/stat`, 'latin1');
      const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
      if (fields[0] === 'Z' && Number(fields[1]) === root) out.push(Number(name));
    } catch {}
  }
  return out;
}

export { existsSync };
