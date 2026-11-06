// The production stack, as Docker Compose runs it: the images from infrastructure/docker, wired up the way production.yml
// says, recording and ingesting real media, with media going through the published ports like anyone's would.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { get as httpsGet } from 'node:https';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import {
  Api, RTMP_PORT, assertAudioFlows, assertVideoFlows, audioStats, download, ffprobe, joinRoom, repo, sleep, startBrowser, tmp, videoStats, waitFor, waitForStatus, waitForTracks,
} from './lib.mjs';

const run = promisify(execFile);
const PROJECT = 'streamkit-media-compose';
const envFile = join(tmp, 'compose.env');
const composeArgs = ['compose', '-p', PROJECT, '-f', join(repo, 'infrastructure/compose/production.yml'), '-f', join(repo, 'tests/media/compose/override.yml'), '--env-file', envFile];
const compose = (...args) => run('docker', [...composeArgs, ...args], { maxBuffer: 1 << 26, timeout: 15 * 60 * 1000 });
const SERVICES = ['postgres', 'redis', 'server', 'egress', 'ingress', 'dashboard', 'meeting', 'grafana', 'nginx'];
// The published nginx port. Its certificate is one made here, so this file leaves Node and Chrome not checking it.
const EDGE = 'https://127.0.0.1:57443';
const EDGE_WS = 'wss://127.0.0.1:57443';

let stack;
let api;
let env;

// The address other machines would use to reach this one: media is sent here, not to localhost.
async function hostAddress() {
  const { stdout } = await run('ip', ['-4', 'route', 'get', '1.1.1.1']);
  return /src (\d+\.\d+\.\d+\.\d+)/.exec(stdout)[1];
}

before(async () => {
  mkdirSync(tmp, { recursive: true });
  process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
  const tls = join(tmp, 'tls');
  mkdirSync(tls, { recursive: true });
  await run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=streamkit.test', '-keyout', join(tls, 'privkey.pem'), '-out', join(tls, 'fullchain.pem')]);
  chmodSync(join(tls, 'privkey.pem'), 0o644);
  const secret = () => randomBytes(24).toString('hex');
  env = {
    STREAMKIT_DOMAIN: 'streamkit.test', STREAMKIT_PUBLIC_IP: await hostAddress(), STREAMKIT_TLS_DIR: tls, STREAMKIT_MASTER_KEY: randomBytes(32).toString('hex'),
    STREAMKIT_DB_PASSWORD: secret(), STREAMKIT_WORKER_DB_PASSWORD: secret(), STREAMKIT_REDIS_PASSWORD: secret(), STREAMKIT_TURN_SECRET: secret(), STREAMKIT_METRICS_TOKEN: secret(), STREAMKIT_GRAFANA_PASSWORD: secret(),
  };
  writeFileSync(envFile, Object.entries(env).map(([k, v]) => `${k}=${v}`).join('\n'));
  await compose('down', '-v', '--remove-orphans').catch(() => {});
  // Builds run under dockerd too, so they need a limit of their own.
  await compose('build', '--memory', '6g');
  await compose('up', '-d', ...SERVICES);
  stack = { chrome: await startBrowser({ args: ['--ignore-certificate-errors'] }) };
  await waitFor('the server', async () => (await fetch('http://127.0.0.1:57880/livez')).ok, { timeout: 60000 });
  api = await Api.signUp('compose');
  stack.api = api;
  await waitFor('both workers to report healthy', async () => {
    const { items } = await api.ok('GET', '/servers');
    const healthy = (kind) => items.some((s) => s.stats.kind === kind && s.stats.healthy && s.stats.max_jobs > 0);
    return healthy('egress_worker') && healthy('ingress_worker');
  }, { timeout: 90000 });
}, { timeout: 20 * 60 * 1000 });

after(async () => {
  await stack?.chrome?.browser.close();
  stack?.chrome?.pages.close();
  if (process.env.COMPOSE_KEEP) return;
  const { stdout } = await compose('logs', '--no-color', '--tail', '200', 'server', 'egress', 'ingress').catch(() => ({ stdout: '' }));
  writeFileSync(join(tmp, 'compose.log'), stdout);
  await compose('down', '-v', '--remove-orphans').catch(() => {});
});

const inContainer = async (service, script) => (await compose('exec', '-T', service, 'sh', '-c', script)).stdout;

describe('the production stack in Docker', () => {
  it('records a room from inside a container, with Chrome\'s sandbox on, and without a display or a sound server', async () => {
    const room = `compose-${process.pid}`;
    const a = await joinRoom(stack, api, 'alice', room);
    const b = await joinRoom(stack, api, 'bob', room);
    try {
      await waitForTracks(a, 'bob', ['audio', 'video']);
      await assertVideoFlows(a, 'bob');
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, layout: 'grid', output: { kind: 'file', format: 'mp4' } }, [201]);
      await waitForStatus(api, job.id, 'active', 60000);

      // The sandbox has to be really on: a renderer in its own user and PID namespaces, under seccomp, and not able to see
      // what the worker can.
      const report = await inContainer('egress', `
        for d in /proc/[0-9]*; do
          # By how it starts, because this script has the same words in its own command line.
          case "$(tr '\\0' ' ' < $d/cmdline 2>/dev/null)" in /opt/google/chrome/chrome*--type=renderer*) ;; *) continue ;; esac
          echo "renderer $d"
          tr '\\0' ' ' < $d/cmdline | grep -c -- '--no-sandbox'
          grep -E '^(NSpid|Seccomp|NoNewPrivs):' $d/status
          readlink $d/ns/user
          readlink $d/root
        done
        echo "worker user namespace $(readlink /proc/self/ns/user)"
      `).catch((e) => e.stdout ?? String(e));
      console.log(`  ${report.replace(/\s+/g, ' ')}`);
      assert.match(report, /renderer/, 'no renderer found');
      assert.doesNotMatch(report, /^[1-9]\d*$/m, 'a renderer was started with --no-sandbox');
      assert.match(report, /Seccomp:\s+2/);
      assert.match(report, /NSpid:\s+\d+\s+\d+/, 'a renderer should be in its own PID namespace');

      await sleep(8000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 60000);
      assert.equal(done.status, 'complete', done.error);
      // The server container reads what the worker container wrote, through the volume they share.
      const path = await download(api, job.id, `${job.id}-compose.mp4`);
      const info = await ffprobe(path);
      assert.equal(info.video.codec_name, 'h264', 'the image should have an H.264 encoder in Chrome');
      assert.equal(info.audio.codec_name, 'aac');
      assert.equal(info.video.width, 1280);
      const whole = await videoStats(path);
      assert.ok(whole.frames > 100 && whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
      assert.ok((await audioStats(path)).peak > -30);
      console.log(`  recorded in a container: ${info.duration.toFixed(2)}s, ${whole.frames} frames`);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('the master key cannot be read from inside a worker\'s container, and ffmpeg and Chrome are limited in what they can touch', async () => {
    for (const service of ['egress', 'ingress']) {
      const out = await inContainer(service, 'id -u; cat /proc/1/environ 2>&1 | tr "\\0" "\\n"; cat /proc/1/mem 2>&1 | head -c 100 | wc -c; true');
      console.log(`  ${service}: ${out.replace(/\s+/g, ' ').slice(0, 120)}`);
      assert.match(out, /^10001/, 'it should not run as root');
      assert.match(out, /Permission denied/);
      assert.ok(!out.includes(env.STREAMKIT_MASTER_KEY) && !out.includes(env.STREAMKIT_WORKER_DB_PASSWORD), `${service}: the worker's secrets can be read`);
    }
  });

  it('both workers really have a read-only root, with only what they need to write to', async () => {
    // A write as the container's root user, so that what stops it is the mount, and not the permissions on the directory.
    const writeAsRoot = async (service, path) => {
      const { stdout } = await compose('exec', '-T', '-u', '0', service, 'sh', '-c', `touch ${path} 2>&1 && echo WROTE; true`);
      return stdout;
    };
    for (const service of ['egress', 'ingress']) {
      const mounts = await inContainer(service, 'cat /proc/self/mounts');
      const root = mounts.split('\n').find((l) => l.split(' ')[1] === '/');
      console.log(`  ${service}: ${root.split(' ').slice(0, 4).join(' ').slice(0, 60)}`);
      assert.ok(root.split(' ')[3].split(',').includes('ro'), `${service}: the root is not mounted read-only: ${root}`);
      for (const path of ['/etc/planted', '/usr/local/bin/planted', '/planted', '/var/planted']) {
        const out = await writeAsRoot(service, path);
        assert.ok(!out.includes('WROTE'), `${service}: ${path} could be written`);
        assert.match(out, /Read-only file system/, `${service}: ${path}: ${out}`);
      }
      // What it does need still works, as the user it runs as.
      assert.match(await inContainer(service, 'touch /tmp/probe && rm /tmp/probe && echo WROTE'), /WROTE/, `${service}: /tmp`);
    }
    assert.match(await inContainer('egress', 'touch /var/lib/streamkit/work/.probe /var/lib/streamkit/recordings/.probe && rm /var/lib/streamkit/work/.probe /var/lib/streamkit/recordings/.probe && echo WROTE'), /WROTE/, 'the volumes');
  });

  it('the workers have no config file, and the configuration they get holds no secrets', async () => {
    for (const service of ['egress', 'ingress']) {
      assert.match(await inContainer(service, 'ls /etc/streamkit 2>&1; true'), /No such file/, `${service} has a config file`);
    }
    const production = readFileSync(join(repo, 'infrastructure/compose/production.yml'), 'utf8');
    const config = production.slice(production.indexOf('x-config:'), production.indexOf('services:'));
    for (const name of ['MASTER_KEY', 'DB_PASSWORD', 'REDIS_PASSWORD', 'TURN_SECRET', 'METRICS_TOKEN', 'GRAFANA_PASSWORD', 'SMTP_PASSWORD']) {
      assert.ok(!config.includes(name), `the shared configuration uses ${name}`);
    }
    assert.ok(config.length > 200, 'this is not the configuration');
  });

  it('workers connect as a role of their own, and can reach only the server and the database', async () => {
    const { stdout } = await compose('exec', '-T', 'postgres', 'psql', '-U', 'streamkit', '-d', 'streamkit', '-At', '-c',
      `SELECT usename, count(*) FROM pg_stat_activity WHERE datname = 'streamkit' AND pid <> pg_backend_pid() GROUP BY 1 ORDER BY 1`);
    console.log(`  connections: ${stdout.replace(/\s+/g, ' ')}`);
    assert.match(stdout, /streamkit_worker\|[1-9]/, 'the workers should connect as streamkit_worker');
    const roles = await compose('exec', '-T', 'postgres', 'psql', '-U', 'streamkit', '-d', 'streamkit', '-At', '-c', `SELECT rolsuper, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = 'streamkit_worker'`);
    assert.equal(roles.stdout.trim(), 'f|f|f');
    // The stand-ins are on the internal network, with the dashboard's name.
    const names = await inContainer('egress', 'for h in postgres server redis grafana dashboard nginx; do getent hosts $h >/dev/null && echo "$h resolves" || echo "$h does not"; done');
    console.log(`  ${names.replace(/\s+/g, ' ')}`);
    assert.match(names, /postgres resolves/);
    assert.match(names, /server resolves/);
    for (const other of ['redis', 'grafana', 'dashboard', 'nginx']) assert.match(names, new RegExp(`${other} does not`));
  });

  it('a worker in a container has no dead processes piling up', async () => {
    const count = `n=0; for d in /proc/[0-9]*; do s=$(sed 's/.*) //' $d/stat 2>/dev/null) || continue; case "$s" in Z*) n=$((n+1));; esac; done; echo $n`;
    const until = Date.now() + 30000;
    let zombies;
    do {
      zombies = Number((await inContainer('egress', count)).trim());
      if (zombies === 0) break;
      await sleep(2000);
    } while (Date.now() < until);
    assert.equal(zombies, 0, `${zombies} dead processes with nothing waiting for them, after a recording`);
  }, { timeout: 60000 });

  it('a clean stop of the container finishes the recording first', async () => {
    const room = `compose-stop-${process.pid}`;
    const a = await joinRoom(stack, api, 'alice', room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
      await waitForStatus(api, job.id, 'active', 60000);
      await sleep(5000);
      await compose('stop', 'egress');
      const done = await api.ok('GET', `/egress/${job.id}`);
      assert.equal(done.status, 'complete', done.error);
      assert.ok(done.file_size > 0);
      assert.equal((await api.req('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } })).status, 503, 'with the worker gone, the API should say so');
      await compose('start', 'egress');
      await waitFor('the worker to return', async () => {
        const { items } = await api.ok('GET', '/servers');
        return items.some((s) => s.stats.kind === 'egress_worker' && s.stats.healthy && s.stats.max_jobs > 0 && Date.now() - Date.parse(s.last_seen_at) < 15000);
      }, { timeout: 60000 });
    } finally {
      await a.close();
    }
  });

  it('takes an RTMP stream through the published port, and a subscriber decodes it', async () => {
    const room = `compose-rtmp-${process.pid}`;
    const ingress = await api.ok('POST', '/ingress', { name: 'Main stage', input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    assert.equal(ingress.publish_url, `rtmp://127.0.0.1:${RTMP_PORT}/live`);
    const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
    const pusher = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
      '-c:v', 'libx264', '-preset', 'veryfast', '-bf', '2', '-g', '60', '-b:v', '1500k', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-f', 'flv', `${ingress.publish_url}/${ingress.stream_key}`], { stdio: 'ignore' });
    try {
      await waitFor('publishing', async () => (await api.ok('GET', `/ingress/${ingress.id}`)).status === 'publishing', { timeout: 45000 });
      await waitForTracks(viewer, 'stage', ['audio', 'video']);
      const video = await assertVideoFlows(viewer, 'stage');
      await assertAudioFlows(viewer, 'stage');
      console.log(`  a stream through the ingress container: ${video.frames} frames in 2.5s, ${video.width}x${video.height}`);
    } finally {
      pusher.kill('SIGKILL');
      await viewer.close();
    }
  });

  it('a WHIP publisher reaches the server container', async () => {
    const room = `compose-whip-${process.pid}`;
    const ingress = await api.ok('POST', '/ingress', { input_type: 'whip', room, participant_identity: 'whip-pub' }, [201]);
    const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
    const ctx = await stack.chrome.browser.newContext({ permissions: ['camera', 'microphone'] });
    const page = await ctx.newPage();
    await page.goto(stack.chrome.base);
    try {
      const status = await page.evaluate(async ({ key, endpoint }) => {
        const pc = new RTCPeerConnection();
        window.pc = pc;
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: { width: 640, height: 360 } });
        for (const track of stream.getTracks()) pc.addTransceiver(track, { direction: 'sendonly', streams: [stream] });
        await pc.setLocalDescription(await pc.createOffer());
        const res = await fetch(endpoint, { method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/sdp' }, body: pc.localDescription.sdp });
        const answer = await res.text();
        if (res.status === 201) await pc.setRemoteDescription({ type: 'answer', sdp: answer });
        return res.status;
      }, { key: ingress.stream_key, endpoint: ingress.publish_url });
      assert.equal(status, 201);
      await waitForTracks(viewer, 'whip-pub', ['audio', 'video']);
      await assertVideoFlows(viewer, 'whip-pub');
    } finally {
      await ctx.close();
      await viewer.close();
    }
  });

  it('with the seccomp profile left off in a container, the worker says what to do instead of failing every job', async () => {
    const yaml = join(tmp, 'sandbox-on.yaml');
    writeFileSync(yaml, 'media:\n  ws_url: ws://server:7880\n  storage:\n    dir: /tmp/recordings\n  egress:\n    work_dir: /tmp/work\n');
    const { stdout: image } = await compose('images', '-q', 'egress');
    const result = await run('docker', ['run', '--rm', '--network', `${PROJECT}_media`, '--memory', '1g', '--cap-drop', 'ALL',
      '-e', `STREAMKIT_DATABASE_URL=postgres://streamkit_worker:${env.STREAMKIT_WORKER_DB_PASSWORD}@postgres:5432/streamkit?sslmode=disable`, '-e', `STREAMKIT_MASTER_KEY=${env.STREAMKIT_MASTER_KEY}`,
      '-v', `${yaml}:/c.yaml:ro`, image.trim().split('\n')[0], '--config', '/c.yaml'], { timeout: 120000 }).catch((e) => e);
    assert.notEqual(result.code ?? 0, 0, 'it should refuse to start');
    console.log(`  ${String(result.stderr).trim().slice(0, 300)}`);
    assert.match(String(result.stderr), /chrome-seccomp\.json/);
  }, { timeout: 180000 });
});

// Traffic through infrastructure/nginx/streamkit.conf.template: the only way in that production publishes for the API, signalling
// and recordings. Media goes straight to the server, as the template says, on the published ports.
describe('through nginx', () => {
  let edge;
  let job;

  before(async () => {
    await waitFor('nginx', async () => (await fetch(`${EDGE}/livez`).catch(() => null))?.status > 0, { timeout: 30000 });
    edge = await Api.signUp('edge', EDGE);
    stack.edge = edge;
  });

  it('answers on HTTPS, redirects nothing it should serve, and keeps metrics inside', async () => {
    const api = await fetch(`${EDGE}/api/v1/auth/me`);
    assert.notEqual(api.status, 502, 'the API should be reachable through nginx');
    assert.match(api.headers.get('strict-transport-security') ?? '', /max-age/);
    assert.equal(api.headers.get('x-content-type-options'), 'nosniff');
    assert.equal((await fetch(`${EDGE}/metrics`)).status, 404);
  });

  it('carries signalling for people in a call, with their media going straight to the server', async () => {
    const room = `edge-${process.pid}`;
    const a = await joinRoom(stack, edge, 'alice', room, { ws: EDGE_WS });
    const b = await joinRoom(stack, edge, 'bob', room, { ws: EDGE_WS });
    try {
      await waitForTracks(a, 'bob', ['audio', 'video']);
      await waitForTracks(b, 'alice', ['audio', 'video']);
      const seen = await assertVideoFlows(a, 'bob');
      await assertAudioFlows(a, 'bob');
      console.log(`  a call signalled through nginx: ${seen.frames} frames in 2.5s`);
      // Longer than nginx's default of 60 seconds with nothing to read, which is how a location that forgot its timeout fails.
      await sleep(70000);
      await assertVideoFlows(a, 'bob');
      assert.equal(await a.page.$eval('#state', (e) => e.dataset.state), 'connected');

      job = await edge.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
      await waitForStatus(edge, job.id, 'active', 60000);
      await sleep(6000);
      await edge.ok('POST', `/egress/${job.id}/stop`, {});
      job = await waitForStatus(edge, job.id, ['complete', 'failed'], 60000);
      assert.equal(job.status, 'complete', job.error);
    } finally {
      await a.close();
      await b.close();
    }
  }, { timeout: 240000 });

  it('serves a recording whole and in ranges, unbuffered, and a browser can seek in it', async () => {
    assert.ok(job, 'the test before this one makes the recording');
    const headers = { Cookie: edge.cookie, 'X-Project-ID': edge.project };
    const whole = await fetch(`${EDGE}/api/v1/recordings/${job.id}/file`, { headers });
    assert.equal(whole.status, 200);
    assert.equal(whole.headers.get('content-type'), 'video/mp4');
    const data = Buffer.from(await whole.arrayBuffer());
    assert.equal(data.length, job.file_size);
    const path = join(tmp, `${job.id}-edge.mp4`);
    writeFileSync(path, data);
    assert.equal((await ffprobe(path)).video.codec_name, 'h264');

    const part = await fetch(`${EDGE}/api/v1/recordings/${job.id}/file`, { headers: { ...headers, Range: 'bytes=1000-1999' } });
    assert.equal(part.status, 206);
    assert.equal(part.headers.get('content-range'), `bytes 1000-1999/${job.file_size}`);
    assert.ok(Buffer.from(await part.arrayBuffer()).equals(data.subarray(1000, 2000)), 'the range holds the wrong bytes');
    const tail = await fetch(`${EDGE}/api/v1/recordings/${job.id}/file`, { headers: { ...headers, Range: 'bytes=-500' } });
    assert.equal(tail.status, 206);
    assert.ok(Buffer.from(await tail.arrayBuffer()).equals(data.subarray(data.length - 500)));
    const bad = await fetch(`${EDGE}/api/v1/recordings/${job.id}/file`, { headers: { ...headers, Range: `bytes=${job.file_size + 10}-` } });
    assert.equal(bad.status, 416);
    const other = await Api.signUp('edge-other', EDGE);
    assert.equal((await other.req('GET', `/recordings/${job.id}/file`)).status, 404, 'another project must not get it through nginx either');

    const ctx = await stack.chrome.browser.newContext({ ignoreHTTPSErrors: true });
    const [name, value] = edge.cookie.split('=');
    await ctx.addCookies([{ name, value, domain: '127.0.0.1', path: '/' }]);
    const page = await ctx.newPage();
    await page.goto(`${EDGE}/livez`);
    const played = await page.evaluate(async (src) => {
      const el = document.createElement('video');
      el.muted = true;
      document.body.appendChild(el);
      el.src = src;
      await new Promise((resolve, reject) => {
        el.onloadedmetadata = resolve;
        el.onerror = () => reject(new Error(`cannot load: ${el.error?.message}`));
      });
      await el.play();
      await new Promise((r) => setTimeout(r, 1000));
      const advanced = el.currentTime;
      el.currentTime = el.duration - 1;
      await new Promise((resolve, reject) => {
        el.onseeked = resolve;
        setTimeout(() => reject(new Error('seek timed out')), 10000);
      });
      return { advanced, seekedTo: el.currentTime, duration: el.duration };
    }, `${EDGE}${job.output_url}?project_id=${edge.project}`);
    await ctx.close();
    console.log(`  played through nginx: ${JSON.stringify(played)}`);
    assert.ok(played.advanced > 0.3);
    assert.ok(Math.abs(played.seekedTo - (played.duration - 1)) < 0.5);
  }, { timeout: 120000 });

  it('sends bytes of a recording as they come, rather than holding the whole file', async () => {
    assert.ok(job, 'the test before this one makes the recording');
    const body = await new Promise((resolve, reject) => {
      const req = httpsGet(`${EDGE}/api/v1/recordings/${job.id}/file`, { rejectUnauthorized: false, headers: { Cookie: edge.cookie, 'X-Project-ID': edge.project } }, (res) => {
        const started = Date.now();
        res.once('data', () => resolve({ status: res.statusCode, firstByteMs: Date.now() - started }));
        res.on('error', reject);
      });
      req.on('error', reject);
    });
    assert.equal(body.status, 200);
    assert.ok(body.firstByteMs < 5000, JSON.stringify(body));
  });

  it('carries a WHIP publisher, and its session can be ended the same way', async () => {
    const room = `edge-whip-${process.pid}`;
    const ingress = await edge.ok('POST', '/ingress', { input_type: 'whip', room, participant_identity: 'whip-pub' }, [201]);
    const endpoint = `${EDGE}/whip/v1`;
    const viewer = await joinRoom(stack, edge, 'viewer', room, { publish: false, ws: EDGE_WS });
    const ctx = await stack.chrome.browser.newContext({ permissions: ['camera', 'microphone'], ignoreHTTPSErrors: true });
    const page = await ctx.newPage();
    await page.goto(stack.chrome.base);
    try {
      const result = await page.evaluate(async ({ key, endpoint }) => {
        const pc = new RTCPeerConnection();
        window.pc = pc;
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: { width: 640, height: 360 } });
        for (const track of stream.getTracks()) pc.addTransceiver(track, { direction: 'sendonly', streams: [stream] });
        await pc.setLocalDescription(await pc.createOffer());
        const res = await fetch(endpoint, { method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/sdp' }, body: pc.localDescription.sdp });
        const answer = await res.text();
        if (res.status === 201) await pc.setRemoteDescription({ type: 'answer', sdp: answer });
        return { status: res.status, location: res.headers.get('location') };
      }, { key: ingress.stream_key, endpoint });
      assert.equal(result.status, 201);
      assert.match(result.location, /^\/whip\/v1\/WH_/);
      await waitForTracks(viewer, 'whip-pub', ['audio', 'video']);
      await assertVideoFlows(viewer, 'whip-pub');
      assert.equal((await fetch(`${EDGE}${result.location}`, { method: 'PATCH' })).status, 405);
      const del = await fetch(`${EDGE}${result.location}`, { method: 'DELETE', headers: { Authorization: `Bearer ${ingress.stream_key}` } });
      assert.equal(del.status, 200);
      await waitFor('the session to end', async () => (await edge.ok('GET', `/ingress/${ingress.id}`)).status === 'inactive', { timeout: 20000 });
    } finally {
      await ctx.close();
      await viewer.close();
    }
  }, { timeout: 120000 });

  it('holds back an oversized request itself', async () => {
    const big = await fetch(`${EDGE}/whip/v1`, { method: 'POST', headers: { Authorization: 'Bearer x', 'Content-Type': 'application/sdp' }, body: Buffer.alloc(3 << 20, 97) });
    assert.equal(big.status, 413);
  });

  it('carries the live monitor connection, which is a WebSocket on its own location', async () => {
    // What the dashboard's monitor page uses. A browser here would be refused for its origin, as it should be, since that has to be
    // the configured dashboard address, and these tests are not on it. So this connects as a program does, with no origin.
    const result = await new Promise((resolve) => {
      const started = Date.now();
      const ws = new WebSocket(`${EDGE_WS}/api/v1/monitor/live`, { headers: { Cookie: edge.cookie, 'X-Project-ID': edge.project } });
      ws.onmessage = (e) => { resolve({ ms: Date.now() - started, bytes: String(e.data).length }); ws.close(); };
      ws.onerror = () => resolve({ error: 'could not connect' });
      setTimeout(() => resolve({ error: 'nothing came through within 20 seconds' }), 20000);
    });
    console.log(`  live monitor through nginx: ${JSON.stringify(result)}`);
    assert.equal(result.error, undefined);
    assert.ok(result.bytes > 0);
  }, { timeout: 40000 });
});
