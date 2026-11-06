import { spawn } from 'node:child_process';
import { createHash, createHmac } from 'node:crypto';
import { readdirSync, readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { join } from 'node:path';
import { chromium } from 'playwright-core';
import { startFixtureServer } from '../../interoperability/oracle/fixture-server.mjs';
import { DASH, mailDir, WS_URL } from './stack.mjs';

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export const PASSWORD = 'a-good-long-password';

export async function launch(extraArgs = []) {
  return chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: ['--use-fake-device-for-media-stream', ...extraArgs, '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
}

export async function eventually(what, fn, { timeout = 20000, interval = 200 } = {}) {
  const deadline = Date.now() + timeout;
  let last;
  while (Date.now() < deadline) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (err) {
      last = err;
    }
    await sleep(interval);
  }
  throw new Error(`Timed out waiting for ${what}${last ? `: ${last.message}` : ''}`);
}

/** The newest mail to an address, as the server wrote it to the mail directory. */
export function newestMail(email) {
  const suffix = `${email.replace('@', '_at_')}.eml`;
  const files = readdirSync(mailDir).filter((f) => f.endsWith(suffix)).sort();
  if (files.length === 0) return null;
  return readFileSync(join(mailDir, files[files.length - 1]), 'utf8');
}

export async function mailLink(email, path, previous = null) {
  return eventually(`a ${path} email to ${email}`, () => {
    const mail = newestMail(email);
    if (!mail || mail === previous) return null;
    const m = mail.match(new RegExp(`http://[^\\s]+${path}\\?token=[A-Za-z0-9_-]+`));
    return m?.[0] ?? null;
  });
}

/** Calls the API from inside a signed-in page, the way the dashboard does, with its cookie. */
export async function pageApi(page, method, path, { body, project } = {}) {
  return page.evaluate(
    async ({ method, path, body, project }) => {
      const headers = { 'Content-Type': 'application/json' };
      if (project) headers['X-Project-ID'] = project;
      const res = await fetch(`/api/v1${path}`, { method, headers, body: method === 'GET' ? undefined : JSON.stringify(body ?? {}) });
      const text = await res.text();
      let json = null;
      try {
        json = JSON.parse(text);
      } catch {
        // Not every response is JSON.
      }
      return { status: res.status, json, text };
    },
    { method, path, body, project },
  );
}

export async function currentProject(page) {
  return page.evaluate(() => JSON.parse(localStorage.getItem('streamkit.project') ?? '{}').state?.projectId ?? null);
}

/** Auth routes allow a burst of 10 and then 20 a minute, per address, and everything here shares one address. */
export async function submitAuthForm(page, submit, { success, errorText = 'Too many requests' }) {
  for (let i = 0; i < 12; i++) {
    await submit();
    const outcome = await Promise.race([
      success().then(() => 'ok'),
      page.getByText(errorText).first().waitFor({ state: 'visible', timeout: 30000 }).then(() => 'limited'),
    ]);
    if (outcome === 'ok') return;
    await sleep(3500);
  }
  throw new Error('Still rate limited after waiting.');
}

export async function register(page, { email, name = 'Test User', organization = 'Test org', password = PASSWORD }) {
  await page.goto(`${DASH}/register`);
  await page.getByLabel('Your name').fill(name);
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Organization').fill(organization);
  await page.getByLabel('Password').fill(password);
  await submitAuthForm(page, () => page.getByRole('button', { name: 'Create account' }).click(), {
    success: () => page.waitForURL('**/overview', { timeout: 30000 }),
  });
}

export async function signIn(page, { email, password = PASSWORD }) {
  await page.goto(`${DASH}/login`);
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(password);
  await submitAuthForm(page, () => page.getByRole('button', { name: 'Sign in' }).click(), {
    success: () => page.waitForURL('**/overview', { timeout: 30000 }),
  });
}

/** Creates a project through the API and selects it, for tests that are about something else. */
export async function useNewProject(page, name, environment = 'development') {
  const res = await pageApi(page, 'POST', '/projects', { body: { name, environment } });
  if (res.status !== 201) throw new Error(`could not create project: ${res.text}`);
  await page.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), res.json.id);
  return res.json;
}

export async function createKey(page, project, name = 'test key') {
  const res = await pageApi(page, 'POST', '/keys', { project, body: { name } });
  if (res.status !== 201) throw new Error(`could not create key: ${res.text}`);
  return res.json;
}

export function verifyJwt(token, secret) {
  const [head, body, sig] = token.split('.');
  const expected = createHmac('sha256', secret).update(`${head}.${body}`).digest('base64url');
  if (sig !== expected) return null;
  return JSON.parse(Buffer.from(body, 'base64url').toString());
}

export function sha256Base64(body) {
  return createHash('sha256').update(body).digest('base64');
}

/** A real HTTP endpoint for webhooks to reach. Set `.respondWith` to choose its answer. */
export async function startReceiver() {
  const receiver = { received: [], respondWith: 200 };
  const server = createServer((req, res) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      receiver.received.push({ headers: req.headers, body: Buffer.concat(chunks).toString(), answeredWith: receiver.respondWith });
      res.statusCode = receiver.respondWith;
      res.end();
    });
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  receiver.url = `http://127.0.0.1:${server.address().port}/hook`;
  receiver.close = () => server.close();
  return receiver;
}

/** A real participant: the interoperability harness's client, in Chrome, with a fake camera and microphone. */
export class Participants {
  constructor(browser) {
    this.browser = browser;
    this.fixtures = [];
    this.contexts = [];
  }

  async join({ apiKey, apiSecret, identity, room, publish = true }) {
    const { server, port } = await startFixtureServer({ apiKey, apiSecret });
    this.fixtures.push(server);
    const ctx = await this.browser.newContext({ permissions: ['camera', 'microphone'] });
    this.contexts.push(ctx);
    const page = await ctx.newPage();
    const q = new URLSearchParams({ ws: WS_URL, identity, room, mode: 'dual', publish: publish ? '1' : '0' });
    await page.goto(`http://127.0.0.1:${port}/?${q}`);
    await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
    const state = await page.$eval('#state', (e) => `${e.dataset.state} ${e.textContent}`);
    if (!state.startsWith('connected')) throw new Error(`${identity} failed to connect: ${state}`);
    return page;
  }

  async closeAll() {
    for (const ctx of this.contexts.splice(0)) await ctx.close().catch(() => {});
    for (const server of this.fixtures.splice(0)) server.close();
  }
}

/** Pushes a real test stream to an RTMP address, the way OBS would. Returns the process, to kill when finished. */
export function pushRtmp(url) {
  return spawn(
    'ffmpeg',
    ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
      '-c:v', 'libx264', '-preset', 'veryfast', '-tune', 'zerolatency', '-g', '30', '-b:v', '800k', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-b:a', '96k', '-f', 'flv', url],
    { stdio: 'ignore' },
  );
}

/** Fails if a page pushes the whole document sideways, which is what breaks phone layouts. */
export async function horizontalOverflow(page) {
  return page.evaluate(() => {
    const doc = document.documentElement;
    return doc.scrollWidth > doc.clientWidth + 1 ? { scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth } : null;
  });
}
