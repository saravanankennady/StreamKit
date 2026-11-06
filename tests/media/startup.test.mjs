// How a recording gets going, over and over, for chasing things that only happen now and then: how long until the first
// media, and whether what came out has sound. Run it by itself, with STARTUP_RUNS for how many times, and STARTUP_LOAD for
// how many busy loops to keep running next to it.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import {
  audioStats, buildBinaries, download, joinRoom, prepareTmp, received, setUp, sleep, tearDown, tmp, waitForStatus, waitForTracks, writeConfig,
} from './lib.mjs';

const RUNS = Number(process.env.STARTUP_RUNS ?? 10);
const LOAD = Number(process.env.STARTUP_LOAD ?? 0);
let stack;
let api;
const burners = [];
const chromeLogs = join(tmp, 'chrome-logs');

// STARTUP_CHROME_FLAGS adds flags to the recorder's Chrome, and STARTUP_CHROME_DROP takes one away, through a script set as
// chrome_path, so one flag can be compared with and without on the same code.
function chromeWrapper() {
  const add = process.env.STARTUP_CHROME_FLAGS ?? '';
  const drop = process.env.STARTUP_CHROME_DROP ?? '';
  const logs = process.env.STARTUP_CHROME_LOGS ?? '';
  if (!add && !drop && !logs) return '';
  const path = join(tmp, 'chrome-wrapper.sh');
  mkdirSync(chromeLogs, { recursive: true });
  // Chrome's own log for each job, named after the job, if asked for. It is a lot to read, so only the ones that matter stay.
  writeFileSync(path, `#!/bin/bash
args=()
extra=()
for a in "$@"; do
  case "$a" in
    --user-data-dir=*) job=$(basename "$(dirname "\${a#--user-data-dir=}")"); [ -n "${logs}" ] && extra=(--enable-logging --log-file="${chromeLogs}/$job.log" ${logs}) ;;
  esac
  [ "$a" = "${drop}" ] || args+=("$a")
done
exec google-chrome ${add} "\${extra[@]}" "\${args[@]}"
`);
  chmodSync(path, 0o755);
  return `    chrome_path: ${path}`;
}

before(async () => {
  prepareTmp();
  await buildBinaries();
  mkdirSync(tmp, { recursive: true });
  stack = await setUp({ config: writeConfig('config-startup.yaml', '', { egress: chromeWrapper() }) });
  api = stack.api;
  for (let i = 0; i < LOAD; i++) burners.push(spawn('sh', ['-c', 'while :; do :; done'], { stdio: 'ignore' }));
}, { timeout: 180000 });

after(async () => {
  burners.forEach((b) => b.kill('SIGKILL'));
  await tearDown(stack);
});

// What the worker logged about a job, by time.
function jobLog(id) {
  return readFileSync(join(tmp, 'egress.log'), 'utf8').split('\n').filter((l) => l.includes(id)).map((l) => JSON.parse(l));
}

describe('starting a recording', () => {
  it(`works the same way ${RUNS} times over`, async () => {
    const rows = [];
    for (let i = 0; i < RUNS; i++) {
      const room = `startup-${process.pid}-${i}`;
      const a = await joinRoom(stack, api, 'alice', room);
      const b = await joinRoom(stack, api, 'bob', room);
      try {
        await waitForTracks(a, 'bob', ['audio', 'video']);
        await waitForTracks(b, 'alice', ['audio', 'video']);
        const heardBefore = (await received(a)).find((r) => r.identity === 'bob' && r.kind === 'audio');
        const asked = Date.now();
        const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
        const active = await waitForStatus(api, job.id, ['active', 'failed', 'aborted', 'complete'], 90000);
        const tookMs = Date.now() - asked;
        await sleep(4000);
        await api.ok('POST', `/egress/${job.id}/stop`, {}, [200, 409]);
        const done = await waitForStatus(api, job.id, ['complete', 'failed', 'aborted'], 60000);
        const heardAfter = (await received(a)).find((r) => r.identity === 'bob' && r.kind === 'audio');
        const log = jobLog(job.id);
        const at = (msg) => { const line = log.find((l) => l.msg === msg); return line ? Date.parse(line.time) : 0; };
        const stats = log.filter((l) => l.msg === 'recorder stats');
        const started = at('job started');
        const firstStats = stats[0] ? Date.parse(stats[0].time) - started : -1;
        const firstBytes = stats.find((s) => s.bytes > 0);
        // The page reports every two seconds, so a longer gap is the page not running.
        let longestGap = 0;
        for (let k = 1; k < stats.length; k++) longestGap = Math.max(longestGap, Date.parse(stats[k].time) - Date.parse(stats[k - 1].time));
        const sound = done.file_size > 0 ? await audioStats(await download(api, job.id, `${job.id}-startup.mp4`)) : { peak: -Infinity };
        const row = {
          run: i, status: done.status, askedToActiveMs: tookMs, firstStatsMs: firstStats, firstBytesMs: firstBytes ? Date.parse(firstBytes.time) - started : -1, longestStatsGapMs: longestGap,
          audioState: stats.at(-1)?.audioState, audioPeak: Math.max(...stats.map((s) => s.audioPeak ?? -1)), audioPlaying: stats.at(-1)?.audioPlaying, audioIn: stats.at(-1)?.audioIn, peakDb: sound.peak, error: done.error,
          // Whether people in the room could hear each other at the time, which says whether the sound got as far as the server.
          aliceHeardBob: (heardAfter?.totalAudioEnergy ?? 0) - (heardBefore?.totalAudioEnergy ?? 0),
        };
        rows.push(row);
        console.log(`  ${JSON.stringify(row)}`);
        if (row.peakDb < -30) {
          console.log(`  SILENT ${job.id}:\n${log.map((l) => JSON.stringify(l)).join('\n')}`);
        } else {
          rmSync(join(chromeLogs, `${job.id}.log`), { force: true });
        }
        if (active.status !== 'active') console.log(`  DID NOT START ${job.id}: ${active.status}: ${active.error}`);
      } finally {
        await a.close();
        await b.close();
      }
    }
    assert.deepEqual(rows.filter((r) => r.peakDb < -30), [], 'silent recordings');
    assert.deepEqual(rows.filter((r) => r.status !== 'complete'), [], 'recordings that did not finish');
  }, { timeout: RUNS * 180000 });
});
