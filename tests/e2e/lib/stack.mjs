// Starts what the tests run against: a real streamkit-server on its own ports and database, and the production build of
// the dashboard. Set E2E_REUSE=1 to use a stack you started yourself, while working on a test.
import { spawn, spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { cpSync, createWriteStream, existsSync, mkdirSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pg from 'pg';

const here = dirname(fileURLToPath(import.meta.url));
export const e2eDir = join(here, '..');
export const repoRoot = join(e2eDir, '../..');
export const tmpDir = join(e2eDir, '.tmp');
export const mailDir = join(tmpDir, 'mail');
export const shotsDir = join(tmpDir, 'shots');

// The live WebSocket checks its Origin against public_dashboard_url, so tests use exactly this address.
export const DASH = 'http://localhost:3100';
export const BACKEND = 'http://localhost:17880';
export const WS_URL = 'ws://localhost:17880';
// Share links open on the dashboard's address, and it passes /meet to this, as nginx does in production.
const MEETING_PORT = '3101';

const PG_ADMIN = 'postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable';
const DB_NAME = 'streamkit_dash';

// Each run starts from an empty database, so a fresh key is fine, and no key has to live in the repository.
process.env.STREAMKIT_MASTER_KEY ??= randomBytes(32).toString('hex');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitForHttp(url, name) {
  for (let i = 0; i < 300; i++) {
    try {
      const res = await fetch(url);
      if (res.status < 500) return;
    } catch {
      // Not up yet.
    }
    await sleep(100);
  }
  throw new Error(`${name} did not start: see ${tmpDir}`);
}

async function isUp(url) {
  try {
    await fetch(url);
    return true;
  } catch {
    return false;
  }
}

async function resetDatabase() {
  const client = new pg.Client({ connectionString: PG_ADMIN });
  await client.connect();
  await client.query(`DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)`);
  await client.query(`CREATE DATABASE ${DB_NAME}`);
  await client.end();
}

/** Runs SQL on the test database, for setting up what the API has no route for yet, such as adding a member. */
export async function sql(text, values) {
  const url = new URL(PG_ADMIN);
  url.pathname = `/${DB_NAME}`;
  const client = new pg.Client({ connectionString: url.toString() });
  await client.connect();
  try {
    return await client.query(text, values);
  } finally {
    await client.end();
  }
}

function logFile(name) {
  mkdirSync(tmpDir, { recursive: true });
  return createWriteStream(join(tmpDir, name));
}

function run(cmd, args, cwd, logName, env = {}) {
  const log = logFile(logName);
  const child = spawn(cmd, args, { cwd, env: { ...process.env, ...env }, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout.pipe(log);
  child.stderr.pipe(log);
  return child;
}

const children = [];
let workers = false;

/** Whether real recording and ingest workers were built and started for this run. */
export const workersRunning = () => workers;

function goBuild(output, pkg) {
  // The toolchain sits beside the repository. From a worktree it is further away, and go has to be on the PATH already.
  const toolchain = join(repoRoot, '../.toolchain/env.sh');
  const prepare = existsSync(toolchain) ? `. ${toolchain} && ` : '';
  return spawnSync('bash', ['-c', `${prepare}go build -o ${output} ${pkg}`], { cwd: repoRoot, stdio: 'pipe' });
}

export async function startStack() {
  if (process.env.E2E_REUSE) return;
  for (const [url, name] of [[BACKEND, 'port 17880'], [DASH, 'port 3100'], [`http://localhost:${MEETING_PORT}/meet`, `port ${MEETING_PORT}`]]) {
    if (await isUp(url)) throw new Error(`Something is already listening on ${name}. Stop it, or set E2E_REUSE=1.`);
  }
  rmSync(mailDir, { recursive: true, force: true });
  rmSync(join(tmpDir, 'recordings'), { recursive: true, force: true });
  rmSync(join(tmpDir, 'work'), { recursive: true, force: true });
  mkdirSync(mailDir, { recursive: true });
  mkdirSync(join(tmpDir, 'recordings'), { recursive: true });
  mkdirSync(join(tmpDir, 'work'), { recursive: true });
  await resetDatabase();

  const build = goBuild('bin/streamkit-server-dash', './server/cmd/streamkit-server');
  if (build.status !== 0) {
    // Others work on the server at the same time, so its tree can be mid-change. An earlier build still tests the API.
    if (!existsSync(join(repoRoot, 'bin/streamkit-server-dash'))) throw new Error(`go build failed: ${build.stderr}`);
    console.warn(`go build failed, so using the existing bin/streamkit-server-dash:\n${build.stderr}`);
  }
  children.push(run(join(repoRoot, 'bin/streamkit-server-dash'), ['--dev', '--config', 'tests/e2e/streamkit-dash.yaml'], repoRoot, 'server.log'));
  await waitForHttp(`${BACKEND}/healthz`, 'streamkit-server-dash');

  // Workers for recording and ingest. The tests that need them are skipped if these do not build.
  const egress = goBuild('bin/streamkit-egress-dash', './egress/cmd/streamkit-egress');
  const ingress = goBuild('bin/streamkit-ingress-dash', './ingress/cmd/streamkit-ingress');
  if (egress.status === 0 && ingress.status === 0) {
    const config = ['--dev', '--config', 'tests/e2e/streamkit-dash.yaml'];
    children.push(run(join(repoRoot, 'bin/streamkit-egress-dash'), config, repoRoot, 'egress.log'));
    children.push(run(join(repoRoot, 'bin/streamkit-ingress-dash'), config, repoRoot, 'ingress.log'));
    workers = true;
  } else {
    console.warn(`Media workers did not build, so tests that need them are skipped:\n${egress.stderr}\n${ingress.stderr}`);
  }

  const env = { STREAMKIT_API_URL: BACKEND, NEXT_PUBLIC_STREAMKIT_WS_ORIGIN: WS_URL, STREAMKIT_MEETING_URL: `http://127.0.0.1:${MEETING_PORT}` };
  const meeting = join(repoRoot, 'meeting-app');
  if (!process.env.E2E_SKIP_BUILD) {
    // npm run build, not next build: its prebuild step builds the SDKs the app links to.
    const next = spawnSync('npm', ['run', 'build'], { cwd: meeting, env: { ...process.env, ...env }, stdio: 'pipe' });
    if (next.status !== 0) throw new Error(`meeting app build failed:\n${next.stdout}\n${next.stderr}`);
  }
  const meetingServer = join(meeting, '.next/standalone/meeting-app');
  cpSync(join(meeting, '.next/static'), join(meetingServer, '.next/static'), { recursive: true });
  cpSync(join(meeting, 'public'), join(meetingServer, 'public'), { recursive: true });
  children.push(run('node', ['server.js'], meetingServer, 'meeting.log', { PORT: MEETING_PORT, HOSTNAME: '127.0.0.1' }));
  await waitForHttp(`http://127.0.0.1:${MEETING_PORT}/meet`, 'the meeting app');

  const dashboard = join(repoRoot, 'dashboard');
  if (!process.env.E2E_SKIP_BUILD) {
    const next = spawnSync('npx', ['next', 'build'], { cwd: dashboard, env: { ...process.env, ...env }, stdio: 'pipe' });
    if (next.status !== 0) throw new Error(`next build failed:\n${next.stdout}\n${next.stderr}`);
  }
  // The same standalone server the production image runs, with its assets where the image puts them.
  const standalone = join(dashboard, '.next/standalone/dashboard');
  cpSync(join(dashboard, '.next/static'), join(standalone, '.next/static'), { recursive: true });
  cpSync(join(dashboard, 'public'), join(standalone, 'public'), { recursive: true });
  children.push(run('node', ['server.js'], standalone, 'dashboard.log', { PORT: '3100', HOSTNAME: '127.0.0.1' }));
  await waitForHttp(`${DASH}/login`, 'the dashboard');
}

export async function stopStack() {
  for (const child of children.splice(0)) {
    child.kill('SIGTERM');
    await new Promise((resolve) => {
      child.once('exit', resolve);
      setTimeout(resolve, 5000);
    });
  }
}
