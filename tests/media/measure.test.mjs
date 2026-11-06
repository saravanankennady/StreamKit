// Measures what a recording and an ingest stream really cost, from /proc, for everything below each worker: Chrome, its
// helpers, and ffmpeg. The numbers are for this machine, and these are fake cameras, so a real room with busier pictures
// will cost more. Run it on its own, with nothing else busy, if the numbers matter.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { readdirSync, readFileSync } from 'node:fs';
import { cpus } from 'node:os';
import { after, before, describe, it } from 'node:test';
import {
  RTMP_PORT, buildBinaries, joinRoom, prepareTmp, setUp, sleep, tearDown, waitFor, waitForStatus, waitForTracks,
} from './lib.mjs';

const WINDOW_MS = Number(process.env.MEASURE_SECONDS ?? 30) * 1000;
const SETTLE_MS = 8000;
const TICKS = 100;
const PAGE = 4096;

let stack;
let api;
let counter = 0;
const nextRoom = () => `measure-${process.pid}-${++counter}`;
const results = [];

function procs() {
  const out = new Map();
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = readFileSync(`/proc/${name}/stat`, 'latin1');
      const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
      out.set(Number(name), { ppid: Number(fields[1]), ticks: Number(fields[11]) + Number(fields[12]), rss: Number(fields[21]) * PAGE });
    } catch {}
  }
  return out;
}

function below(root, all) {
  const children = new Map();
  for (const [pid, p] of all) {
    if (!children.has(p.ppid)) children.set(p.ppid, []);
    children.get(p.ppid).push(pid);
  }
  const found = [];
  const stack2 = [...(children.get(root) ?? [])];
  while (stack2.length) {
    const pid = stack2.pop();
    found.push(pid);
    stack2.push(...(children.get(pid) ?? []));
  }
  return found;
}

// Cores used by everything below a process, across a window, and the memory it holds at the end of it.
async function measureBelow(pid, ms) {
  const read = () => {
    const all = procs();
    const pids = below(pid, all);
    return { ticks: pids.reduce((sum, p) => sum + all.get(p).ticks, 0), rss: pids.reduce((sum, p) => sum + all.get(p).rss, 0), count: pids.length, all, pids };
  };
  const start = read();
  const t0 = Date.now();
  await sleep(ms);
  const end = read();
  const seconds = (Date.now() - t0) / 1000;
  // Only processes alive throughout, so one that started or exited in the window cannot skew it.
  let ticks = 0;
  for (const p of end.pids) ticks += end.all.get(p).ticks - (start.all.get(p)?.ticks ?? end.all.get(p).ticks);
  // PSS shares out memory that processes have in common. Adding up RSS would count Chrome's many times over.
  let pss = 0;
  for (const p of end.pids) {
    try {
      pss += Number(/^Pss:\s+(\d+) kB/m.exec(readFileSync(`/proc/${p}/smaps_rollup`, 'latin1'))?.[1] ?? 0);
    } catch {}
  }
  return { cores: ticks / TICKS / seconds, rssMB: pss / 1024, processes: end.count, seconds };
}

async function people(room, n) {
  const list = [];
  for (let i = 0; i < n; i++) list.push(await joinRoom(stack, api, `p${i}`, room));
  await waitForTracks(list[0], `p${n - 1}`, ['audio', 'video'], 60000);
  return list;
}

async function measureJob(label, peopleCount, body) {
  const room = nextRoom();
  const clients = await people(room, peopleCount);
  try {
    const job = await api.ok('POST', '/egress', { room, ...body }, [201]);
    await waitForStatus(api, job.id, 'active', 45000);
    await sleep(SETTLE_MS);
    const m = await measureBelow(stack.egress.pid, WINDOW_MS);
    await api.ok('POST', `/egress/${job.id}/stop`, {});
    const done = await waitForStatus(api, job.id, ['complete', 'failed'], 60000);
    assert.equal(done.status, 'complete', done.error);
    const { items } = await api.ok('GET', `/logs?category=recording&room=${room}`);
    const reported = items.find((l) => l.type === 'egress_completed')?.details ?? {};
    const row = {
      job: label, people: peopleCount, cores: Number(m.cores.toFixed(2)), memory_mb: Math.round(m.rssMB), processes: m.processes,
      worker_reported_avg_cores: Number((reported.avg_cores ?? 0).toFixed(2)), mb_per_minute: Number((done.file_size / (done.duration_ms / 60000) / (1 << 20)).toFixed(1)),
    };
    results.push(row);
    console.log(`  ${JSON.stringify(row)}`);
    assert.ok(m.cores > 0.02, `${label} cost nothing, so the measurement is wrong`);
    assert.ok(reported.cpu_seconds > 0);
  } finally {
    await Promise.all(clients.map((c) => c.close()));
  }
}

before(async () => {
  prepareTmp();
  await buildBinaries();
  stack = await setUp();
  api = stack.api;
}, { timeout: 180000 });

after(async () => {
  console.log(`\nMachine: ${cpus().length} threads, ${cpus()[0].model}`);
  console.table(results);
  await tearDown(stack);
});

describe('what a job costs', () => {
  it('idle worker', async () => {
    const m = await measureBelow(stack.egress.pid, 5000);
    results.push({ job: 'idle worker, no job', people: 0, cores: Number(m.cores.toFixed(2)), memory_mb: Math.round(m.rssMB), processes: m.processes });
  });
  it('composite to MP4, two people', () => measureJob('composite mp4', 2, { type: 'room_composite', layout: 'grid', output: { kind: 'file', format: 'mp4' } }), { timeout: 240000 });
  it('composite to WebM, two people', () => measureJob('composite webm', 2, { type: 'room_composite', layout: 'grid', output: { kind: 'file', format: 'webm' } }), { timeout: 240000 });
  it('one participant', () => measureJob('participant mp4', 2, { type: 'participant', participant: 'p0', output: { kind: 'file', format: 'mp4' } }), { timeout: 240000 });
  it('audio only', () => measureJob('audio only', 2, { type: 'audio_only', output: { kind: 'file', format: 'mp4' } }), { timeout: 240000 });
  it('composite to MP4, six people', () => measureJob('composite mp4', 6, { type: 'room_composite', layout: 'grid', output: { kind: 'file', format: 'mp4' } }), { timeout: 300000 });
  it('composite to MP4 and an RTMP stream, two people', async () => {
    const relay = nextRoom();
    const ingress = await api.ok('POST', '/ingress', { input_type: 'rtmp', room: relay, participant_identity: 'relayed' }, [201]);
    await measureJob('composite mp4 + rtmp', 2, { type: 'room_composite', output: { kind: 'file', format: 'mp4' }, stream_urls: [`${ingress.publish_url}/${ingress.stream_key}`] });
    await api.ok('DELETE', `/ingress/${ingress.id}`, undefined, [204]);
  }, { timeout: 240000 });

  for (const [size, bitrate] of [['640x360', '800k'], ['1280x720', '2500k'], ['1920x1080', '5000k']]) {
    it(`one RTMP stream at ${size}`, async () => {
      const room = nextRoom();
      const ingress = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
      const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
      const pusher = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', `testsrc2=s=${size}:r=30`, '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
        '-c:v', 'libx264', '-preset', 'veryfast', '-b:v', bitrate, '-pix_fmt', 'yuv420p', '-g', '60', '-c:a', 'aac', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${ingress.stream_key}`], { stdio: 'ignore' });
      try {
        await waitFor('publishing', async () => (await api.ok('GET', `/ingress/${ingress.id}`)).status === 'publishing', { timeout: 30000 });
        await sleep(SETTLE_MS);
        const m = await measureBelow(stack.ingress.pid, WINDOW_MS);
        const row = { job: `rtmp ingest ${size} ${bitrate}`, people: 1, cores: Number(m.cores.toFixed(2)), memory_mb: Math.round(m.rssMB), processes: m.processes };
        results.push(row);
        console.log(`  ${JSON.stringify(row)}`);
      } finally {
        pusher.kill('SIGKILL');
        await viewer.close();
      }
    }, { timeout: 180000 });
  }
});
