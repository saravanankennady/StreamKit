// A recording stored in S3: a real MinIO in Docker. Set MEDIA_S3=host:port for one already running. Otherwise this starts
// its own, and removes it afterwards.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { createHash, createHmac } from 'node:crypto';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import {
  BASE, buildBinaries, download, ffprobe, joinRoom, prepareTmp, setUp, sleep, tearDown, tmp, videoStats, waitFor, waitForStatus, waitForTracks, writeConfig,
} from './lib.mjs';

const run = promisify(execFile);
const IMAGE = process.env.MEDIA_S3_IMAGE ?? 'cgr.dev/chainguard/minio:latest';
const CONTAINER = 'streamkit-media-minio-test';
const PORT = 59001;
const ACCESS = 'streamkit-media-test';
const SECRET = 'streamkit-media-test-secret';
const BUCKET = 'streamkit-recordings';
let endpoint = process.env.MEDIA_S3;
let ours = false;
let stack;
let api;

// Lists the bucket directly, with a request signed here, so nothing being tested is involved in the answer.
async function bucketKeys() {
  const sha = (v) => createHash('sha256').update(v).digest('hex');
  const hmac = (key, v) => createHmac('sha256', key).update(v).digest();
  const now = new Date().toISOString().replace(/[:-]|\.\d{3}/g, '');
  const day = now.slice(0, 8);
  const query = 'list-type=2&prefix=tests%2F';
  const headers = `host:${endpoint}\nx-amz-content-sha256:${sha('')}\nx-amz-date:${now}\n`;
  const signed = 'host;x-amz-content-sha256;x-amz-date';
  const canonical = `GET\n/${BUCKET}\n${query}\n${headers}\n${signed}\n${sha('')}`;
  const scope = `${day}/us-east-1/s3/aws4_request`;
  const toSign = `AWS4-HMAC-SHA256\n${now}\n${scope}\n${sha(canonical)}`;
  const key = hmac(hmac(hmac(hmac(`AWS4${SECRET}`, day), 'us-east-1'), 's3'), 'aws4_request');
  const signature = hmac(key, toSign).toString('hex');
  const res = await fetch(`http://${endpoint}/${BUCKET}?${query}`, {
    headers: {
      'x-amz-content-sha256': sha(''), 'x-amz-date': now,
      Authorization: `AWS4-HMAC-SHA256 Credential=${ACCESS}/${scope}, SignedHeaders=${signed}, Signature=${signature}`,
    },
  });
  const text = await res.text();
  assert.equal(res.status, 200, text);
  return [...text.matchAll(/<Key>([^<]+)<\/Key>/g)].map((m) => m[1]);
}

before(async () => {
  prepareTmp();
  await buildBinaries();
  if (!endpoint) {
    await run('docker', ['rm', '-f', CONTAINER]).catch(() => {});
    await run('docker', ['run', '-d', '--name', CONTAINER, '-p', `127.0.0.1:${PORT}:9000`, '-e', `MINIO_ROOT_USER=${ACCESS}`, '-e', `MINIO_ROOT_PASSWORD=${SECRET}`, IMAGE, 'server', '/tmp/data']);
    ours = true;
    endpoint = `127.0.0.1:${PORT}`;
  }
  await waitFor('MinIO', async () => (await fetch(`http://${endpoint}/minio/health/live`)).ok, { timeout: 30000 });
  const config = writeConfig('config-s3.yaml');
  const { readFileSync, writeFileSync } = await import('node:fs');
  const text = readFileSync(config, 'utf8');
  const storage = `  storage:\n    kind: s3\n    s3:\n      endpoint: ${endpoint}\n      bucket: ${BUCKET}\n      prefix: tests\n      access_key: ${ACCESS}\n      secret_key: ${SECRET}\n`;
  writeFileSync(config, text.replace(/  storage:\n    kind: local\n    dir: .*\n/, storage));
  stack = await setUp({ config, ingress: false });
  api = stack.api;
}, { timeout: 240000 });

after(async () => {
  await tearDown(stack);
  if (ours) await run('docker', ['rm', '-f', CONTAINER]).catch(() => {});
});

describe('S3 storage', () => {
  it('stores a recording in MinIO, serves it with Range requests, and deletes it', async () => {
    const room = `s3-${process.pid}`;
    const a = await joinRoom(stack, api, 'alice', room);
    const b = await joinRoom(stack, api, 'bob', room);
    try {
      await waitForTracks(a, 'bob', ['audio', 'video']);
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(6000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);

      // Really in the bucket, under the configured prefix, and not on this disk.
      const keys = await bucketKeys();
      assert.ok(keys.includes(`tests/${api.project}/${job.id}.mp4`), `not in the bucket: ${keys}`);
      assert.equal(existsSync(join(tmp, 'recordings', api.project, `${job.id}.mp4`)), false);

      const path = await download(api, job.id, `${job.id}-s3.mp4`);
      const info = await ffprobe(path);
      assert.equal(info.video.codec_name, 'h264');
      assert.equal(info.audio.codec_name, 'aac');
      const whole = await videoStats(path);
      assert.ok(whole.frames > 60 && whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));

      // Ranges, including one from the middle and one from the very end, come straight from S3.
      const get = (range) => fetch(`${BASE}/api/v1/recordings/${job.id}/file`, { headers: { Cookie: api.cookie, 'X-Project-ID': api.project, Range: range } });
      const whole2 = Buffer.from(await (await fetch(`${BASE}/api/v1/recordings/${job.id}/file`, { headers: { Cookie: api.cookie, 'X-Project-ID': api.project } })).arrayBuffer());
      assert.equal(whole2.length, done.file_size);
      const middle = await get('bytes=1000-1999');
      assert.equal(middle.status, 206);
      assert.equal(middle.headers.get('content-range'), `bytes 1000-1999/${done.file_size}`);
      assert.ok(Buffer.from(await middle.arrayBuffer()).equals(whole2.subarray(1000, 2000)), 'the range holds the wrong bytes');
      const tail = await get('bytes=-500');
      assert.equal(tail.status, 206);
      assert.ok(Buffer.from(await tail.arrayBuffer()).equals(whole2.subarray(whole2.length - 500)));

      // A real <video> over S3.
      const ctx = await stack.chrome.browser.newContext();
      const [name, value] = api.cookie.split('=');
      await ctx.addCookies([{ name, value, domain: '127.0.0.1', path: '/' }]);
      const page = await ctx.newPage();
      await page.goto(`${BASE}/`);
      const played = await page.evaluate(async (src) => {
        const v = document.createElement('video');
        v.muted = true;
        document.body.appendChild(v);
        v.src = src;
        await new Promise((res, rej) => { v.onloadedmetadata = res; v.onerror = () => rej(new Error(`cannot load: ${v.error?.message}`)); });
        await v.play();
        await new Promise((r) => setTimeout(r, 1000));
        const advanced = v.currentTime;
        v.currentTime = v.duration - 1;
        await new Promise((res, rej) => { v.onseeked = res; setTimeout(() => rej(new Error('seek timed out')), 10000); });
        return { advanced, seeked: v.currentTime, width: v.videoWidth };
      }, `${BASE}${done.output_url}?project_id=${api.project}`);
      await ctx.close();
      assert.ok(played.advanced > 0.3, JSON.stringify(played));
      assert.equal(played.width, 1280);

      await api.ok('DELETE', `/recordings/${job.id}`, undefined, [204]);
      assert.equal((await api.req('GET', `/recordings/${job.id}`)).status, 404);
      const left = await bucketKeys();
      assert.ok(!left.some((k) => k.includes(job.id)), `still in the bucket: ${left}`);
    } finally {
      await a.close();
      await b.close();
    }
  });
});
