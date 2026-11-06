// Starts the servers a transport test runs against, and the browser it drives them with. A protocol test only
// counts if the reference server passes it too, so these tests run against both by default.
//
//   node --test transport/codec.test.mjs                     both servers, on their own ports
//   TARGETS=ours node --test transport/codec.test.mjs        only this server
//   OUR_SERVER=...  REFERENCE_SERVER=...                     other binaries
//
// The ports are not the defaults: other work on this machine uses those.
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, openSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { SignalRequest, SignalResponse } from '@livekit/protocol';
import { chromium } from 'playwright-core';
import { startFixtureServer } from '../oracle/fixture-server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
export const apiKey = 'devkey';
export const apiSecret = 'secret';
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const referenceBinary = process.env.REFERENCE_SERVER ?? '/run/media/nsroot/develop/streamin_engine/reference/oracle/bin/x/livekit-server';
const ourBinary = process.env.OUR_SERVER ?? join(here, '../../../bin/streamkit-server');

// Each server gets a port block. The offset keeps parallel test files from colliding, should anyone run them so.
const portBase = Number(process.env.TRANSPORT_PORT_BASE ?? 7980);

const definitions = {
  ours: {
    name: 'ours',
    http: portBase,
    start(extra = {}, dir) {
      const { rtc, ...rest } = extra.ours ?? {};
      const yaml = JSON.stringify({
        port: portBase,
        log_level: process.env.SERVER_LOG_LEVEL ?? 'info',
        rtc: { tcp_port: portBase + 1, udp_port: portBase + 2, ...rtc },
        ...rest,
      });
      const file = join(dir, 'ours.yaml');
      writeFileSync(file, yaml);
      return spawn(ourBinary, ['--dev', '--config', file], { stdio: logStdio('ours') });
    },
  },
  reference: {
    name: 'reference',
    http: portBase + 10,
    start(extra = {}) {
      const yaml = JSON.stringify({
        port: portBase + 10,
        bind_addresses: ['127.0.0.1'],
        development: true,
        keys: { [apiKey]: apiSecret },
        logging: { level: process.env.SERVER_LOG_LEVEL ?? 'info' },
        rtc: { tcp_port: portBase + 11, udp_port: portBase + 12, use_external_ip: false, enable_loopback_candidate: true },
        ...extra.reference,
      });
      return spawn(referenceBinary, ['--config-body', yaml], { stdio: logStdio('reference') });
    },
  },
};

// With SERVER_LOG_DIR set, a server's output goes to a file there. A file, because a pipe nobody reads would
// eventually stop the server.
function logStdio(name) {
  const dir = process.env.SERVER_LOG_DIR;
  if (!dir) return 'ignore';
  const fd = openSync(join(dir, `${name}-${process.pid}.log`), 'a');
  return ['ignore', fd, fd];
}

/** The servers this run covers: `TARGETS=ours,reference` by default. */
export function selectedTargets() {
  return (process.env.TARGETS ?? 'ours,reference')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((name) => {
      const def = definitions[name];
      if (!def) throw new Error(`unknown target ${name}`);
      return def;
    });
}

async function waitForServer(http, child) {
  for (let i = 0; i < 150; i++) {
    if (child.exitCode !== null) throw new Error(`server exited early with code ${child.exitCode}`);
    try {
      const r = await fetch(`${http}/`);
      if (r.ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error('server did not start');
}

/**
 * Starts one server. `extra` holds config for each: `{ ours: {...}, reference: {...} }`. Stops it with stop().
 */
export async function startServer(target, extra) {
  if (target.name === 'reference' && !existsSync(referenceBinary)) throw new Error(`no reference server at ${referenceBinary}`);
  const dir = mkdtempSync(join(tmpdir(), 'sk-transport-'));
  const child = target.start(extra, dir);
  const http = `http://127.0.0.1:${target.http}`;
  try {
    await waitForServer(http, child);
  } catch (e) {
    child.kill('SIGKILL');
    rmSync(dir, { recursive: true, force: true });
    throw e;
  }
  return {
    name: target.name,
    http,
    ws: `ws://127.0.0.1:${target.http}`,
    process: child,
    async stop() {
      if (child.exitCode === null) {
        const gone = new Promise((r) => child.once('exit', r));
        child.kill('SIGTERM');
        await Promise.race([gone, sleep(5000)]);
        if (child.exitCode === null) child.kill('SIGKILL');
      }
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

export async function openBrowser(extraArgs = []) {
  const fixture = await startFixtureServer({ apiKey, apiSecret });
  const browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    // Both fake flags, always: fake UI alone would hand getDisplayMedia the real desktop.
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required', ...extraArgs],
  });
  return {
    browser,
    fixture,
    async close() {
      await browser.close();
      fixture.server.close();
    },
  };
}

/**
 * Opens the test client in its own browser context, with every signalling frame decoded into `frames`, oldest
 * first. Options become the page's URL parameters: see oracle/client.ts.
 */
export async function openClient(env, server, identity, room, params = {}) {
  const ctx = await env.browser.newContext({ permissions: ['camera', 'microphone'], viewport: { width: 1280, height: 720 } });
  const page = await ctx.newPage();
  // A test can ask for its own script to run before the client does.
  if (params.avInit) await page.addInitScript({ path: join(here, '../transport/avsync-page.js') });
  if (params.initScript) await page.addInitScript({ content: params.initScript });
  const { initScript: _unused, ...pageParams } = params;
  const frames = [];
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('websocket', (sock) => {
    frames.push({ dir: 'open', url: sock.url() });
    sock.on('framesent', ({ payload }) => {
      try {
        const msg = SignalRequest.fromBinary(Buffer.from(payload));
        frames.push({ dir: 'c2s', case: msg.message.case, value: msg.message.value });
      } catch {}
    });
    sock.on('framereceived', ({ payload }) => {
      try {
        const msg = SignalResponse.fromBinary(Buffer.from(payload));
        frames.push({ dir: 's2c', case: msg.message.case, value: msg.message.value });
      } catch {}
    });
  });
  const q = new URLSearchParams({ ws: server.ws, identity, room, mode: 'dual', publish: '0', autosub: '1', dynacast: '0', ...pageParams });
  await page.goto(`http://127.0.0.1:${env.fixture.port}/?${q}`);
  await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
  const state = await page.$eval('#state', (e) => `${e.dataset.state} ${e.textContent}`);
  if (!state.startsWith('connected')) {
    await ctx.close();
    throw new Error(`${identity} failed to connect to ${server.name}: ${state}`);
  }
  return { page, ctx, identity, frames, errors, close: () => ctx.close() };
}

export const framesOf = (client, dir, kind) => client.frames.filter((f) => f.dir === dir && f.case === kind).map((f) => f.value);

export const until = (client, fn, arg, timeout = 15000, what = '') =>
  client.page.waitForFunction(fn, arg, { timeout, polling: 100 }).catch((e) => {
    throw new Error(`${client.identity}: timed out waiting for ${what || fn}: ${e.message}`);
  });

/** Video codec names of one SDP media section, in the order listed. */
export function codecsInSection(sdp, predicate) {
  const out = [];
  for (const section of sdp.split(/\r?\n(?=m=)/).slice(1)) {
    if (!predicate(section)) continue;
    const payloads = section.split(/\r?\n/, 1)[0].split(' ').slice(3);
    for (const pt of payloads) {
      const m = section.match(new RegExp(`a=rtpmap:${pt} ([^/]+)/`));
      if (m) out.push(m[1]);
    }
  }
  return out;
}

export const videoCodecsOf = (sdp, mid) => codecsInSection(sdp, (s) => s.startsWith('m=video') && (!mid || s.includes(`a=mid:${mid}\r\n`) || s.includes(`a=mid:${mid}\n`)));

export const reportNumbers = (label, numbers) => console.log(`# ${label}: ${JSON.stringify(numbers)}`);
