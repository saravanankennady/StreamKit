// A real browser talks to a real agent. Chrome publishes real speech from a file, through its own WebRTC stack. The Python worker
// hears it, thinks and answers through local engines. Chrome decodes the answer, and Whisper reads back what it heard.
//
// Latency is measured in the browser, from the last loud moment of its microphone to the first loud moment of the agent's track,
// so it includes the network and jitter buffers. This is on one machine, over loopback.
//
//   npm test                                         our server, then the reference
//   STREAMKIT_TEST_SERVERS=ours npm test
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { createWriteStream, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '../..');
const root = process.env.STREAMKIT_DEV_ROOT ?? '/run/media/nsroot/develop/streamin_engine';
const toolchain = join(root, '.toolchain');
const python = join(toolchain, 'venvs/agents/bin/python');
const enginePython = join(toolchain, 'venvs/agents-engines/bin/python');
const API_KEY = 'devkey';
const API_SECRET = 'secret';
const PORTS = { ours: 60880, reference: 60980 };
const ENGINE = { llm: 60990, stt: 60991, tts: 60992 };
// Capitals Whisper can hear from Piper: 30 of 30 with no WebRTC in the way. "Tokyo" managed 17 of 30, as "Tokuvau" and the like.
const QUESTIONS = { ours: { country: 'Italy', capital: /rome/i }, reference: { country: 'Spain', capital: /madrid/i } };
const servers = (process.env.STREAMKIT_TEST_SERVERS ?? 'ours,reference').split(',').map((s) => s.trim()).filter(Boolean);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const tmp = mkdtempSync(join(process.env.TMPDIR ?? '/tmp', 'sk-agents-browser-'));
process.on('exit', () => rmSync(tmp, { recursive: true, force: true }));

const b64 = (data) => Buffer.from(data).toString('base64url');
function mintToken(claims) {
  const now = Math.floor(Date.now() / 1000);
  const header = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const payload = b64(JSON.stringify({ iss: API_KEY, nbf: now, iat: now, exp: now + 3600, ...claims }));
  return `${header}.${payload}.${b64(createHmac('sha256', API_SECRET).update(`${header}.${payload}`).digest())}`;
}

async function waitHttp(url, proc, timeoutMs = 90000) {
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

function logTo(proc, name) {
  const log = createWriteStream(join(tmp, `${name}.log`));
  proc.stdout?.pipe(log);
  proc.stderr?.pipe(log);
}

function stop(proc) {
  return new Promise((resolve) => {
    if (!proc || proc.exitCode !== null) return resolve();
    proc.once('exit', resolve);
    proc.kill('SIGTERM');
    setTimeout(() => proc.kill('SIGKILL'), 8000).unref();
  });
}

function startServer(name) {
  const port = PORTS[name];
  const body = `port: ${port}\nrtc:\n  tcp_port: ${port + 1}\n  udp_port: ${port + 2}\n`;
  let proc;
  if (name === 'ours') {
    const binary = join(tmp, 'streamkit-server');
    execFileSync(join(toolchain, 'go/bin/go'), ['build', '-o', binary, './server/cmd/streamkit-server'], {
      cwd: repo,
      env: { ...process.env, GOROOT: join(toolchain, 'go'), GOPATH: join(toolchain, 'gopath'), GOTMPDIR: join(toolchain, 'tmp'), GOTOOLCHAIN: 'local' },
    });
    const config = join(tmp, 'ours.yaml');
    writeFileSync(config, body);
    proc = spawn(binary, ['--dev', '--config', config]);
  } else {
    proc = spawn(join(root, 'reference/oracle/bin/x/livekit-server'), ['--dev', '--bind', '127.0.0.1', '--config-body', body]);
  }
  logTo(proc, name);
  return proc;
}

function wavFile(pcm, rate, path) {
  const h = Buffer.alloc(44);
  h.write('RIFF', 0);
  h.writeUInt32LE(36 + pcm.length, 4);
  h.write('WAVEfmt ', 8);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(1, 22);
  h.writeUInt32LE(rate, 24);
  h.writeUInt32LE(rate * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write('data', 36);
  h.writeUInt32LE(pcm.length, 40);
  writeFileSync(path, Buffer.concat([h, pcm]));
}

/** Resamples by averaging, which is plenty for checking words. Whisper wants 16 kHz. */
function to16k(pcm, rate) {
  const samples = new Int16Array(pcm.buffer, pcm.byteOffset, pcm.length >> 1);
  const ratio = rate / 16000;
  const out = Buffer.alloc(Math.floor(samples.length / ratio) * 2);
  for (let i = 0; i < out.length / 2; i++) {
    const start = Math.floor(i * ratio);
    const end = Math.max(start + 1, Math.floor((i + 1) * ratio));
    let sum = 0;
    for (let j = start; j < end; j++) sum += samples[j] ?? 0;
    out.writeInt16LE(Math.round(sum / (end - start)), i * 2);
  }
  return out;
}

async function transcribe(pcm16k) {
  const wav = join(tmp, `check-${Date.now()}.wav`);
  wavFile(pcm16k, 16000, wav);
  const form = new FormData();
  form.append('model', 'whisper');
  form.append('file', new Blob([readFileSync(wav)], { type: 'audio/wav' }), 'speech.wav');
  const res = await fetch(`http://127.0.0.1:${ENGINE.stt}/v1/audio/transcriptions`, { method: 'POST', body: form });
  return (await res.json()).text;
}

describe('a browser talks to an agent', () => {
  const engines = [];
  let pageServer;
  let pageUrl;
  const questionWavs = {};

  before(async () => {
    const local = join(repo, 'streamkit-agents/local-engines');
    const llama = join(toolchain, 'llama.cpp/llama-b12102');
    const stt = spawn(enginePython, [join(local, 'stt_server.py'), '--model', join(toolchain, 'models/whisper/base.en'), '--port', String(ENGINE.stt)]);
    const tts = spawn(enginePython, [join(local, 'tts_server.py'), '--voice', join(toolchain, 'models/piper/en_US-ljspeech-medium.onnx'), '--port', String(ENGINE.tts)]);
    const llm = spawn(join(llama, 'llama-server'), ['-m', join(toolchain, 'models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf'), '--host', '127.0.0.1', '--port', String(ENGINE.llm), '-t', '8', '-c', '4096', '--jinja'], {
      env: { ...process.env, LD_LIBRARY_PATH: llama },
    });
    engines.push(stt, tts, llm);
    logTo(stt, 'stt');
    logTo(tts, 'tts');
    logTo(llm, 'llm');
    await waitHttp(`http://127.0.0.1:${ENGINE.stt}/`, stt);
    await waitHttp(`http://127.0.0.1:${ENGINE.tts}/`, tts);
    await waitHttp(`http://127.0.0.1:${ENGINE.llm}/health`, llm);

    // The first request loads the model into memory, and should not count against whichever server comes first.
    await fetch(`http://127.0.0.1:${ENGINE.llm}/v1/chat/completions`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ model: 'x', messages: [{ role: 'user', content: 'Say hello.' }] }) });

    // The questions, spoken by the speech engine, with silence in front so subscriptions can settle. There is a different one per
    // server, because llama.cpp remembers the last prompt and answers it again in a tenth of the time.
    for (const server of servers) {
      const res = await fetch(`http://127.0.0.1:${ENGINE.tts}/v1/audio/speech`, { method: 'POST', body: JSON.stringify({ input: `What is the capital of ${QUESTIONS[server].country}?`, model: 'piper', voice: 'default' }) });
      const speech = Buffer.from(await res.arrayBuffer());
      const rate = Number(res.headers.get('x-sample-rate'));
      questionWavs[server] = join(tmp, `question-${server}.wav`);
      wavFile(Buffer.concat([Buffer.alloc(rate * 2 * 3), speech]), rate, questionWavs[server]);
    }

    const html = '<!doctype html><title>loading</title><script type="module" src="/client.js"></script>';
    const js = readFileSync(join(here, 'page/dist/client.js'));
    pageServer = createServer((req, res) => {
      if (req.url === '/client.js') {
        res.setHeader('content-type', 'text/javascript');
        res.end(js);
      } else {
        res.setHeader('content-type', 'text/html');
        res.end(html);
      }
    });
    await new Promise((resolve) => pageServer.listen(0, '127.0.0.1', resolve));
    pageUrl = `http://127.0.0.1:${pageServer.address().port}/`;
  });

  after(async () => {
    pageServer?.close();
    await Promise.all(engines.map(stop));
  });

  for (const serverName of servers) {
    it(`real speech both ways on ${serverName}`, { timeout: 180000 }, async () => {
      const server = startServer(serverName);
      let worker;
      let browser;
      try {
        const port = PORTS[serverName];
        await waitHttp(`http://127.0.0.1:${port}/`, server);
        const metricsFile = join(tmp, `metrics-${serverName}.jsonl`);
        const agentName = `browser-agent-${serverName}`;
        worker = spawn(python, [join(repo, 'streamkit-agents/python/examples/local_voice_agent.py'), '--agent-name', agentName, '--metrics-file', metricsFile, '--no-greeting', '--dev'], {
          env: {
            ...process.env,
            STREAMKIT_URL: `ws://127.0.0.1:${port}`,
            STREAMKIT_API_KEY: API_KEY,
            STREAMKIT_API_SECRET: API_SECRET,
            STREAMKIT_LOCAL_LLM_URL: `http://127.0.0.1:${ENGINE.llm}/v1`,
            STREAMKIT_LOCAL_STT_URL: `http://127.0.0.1:${ENGINE.stt}/v1`,
            STREAMKIT_LOCAL_TTS_URL: `http://127.0.0.1:${ENGINE.tts}/v1`,
            SILERO_VAD_MODEL: join(toolchain, 'models/silero/silero_vad.onnx'),
            PYTHONPATH: join(repo, 'streamkit-agents/python/src'),
          },
        });
        logTo(worker, `worker-${serverName}`);
        await new Promise((resolve, reject) => {
          const timer = setTimeout(() => reject(new Error('the worker did not register')), 30000);
          worker.stdout.on('data', (d) => {
            if (String(d).includes('worker registered')) {
              clearTimeout(timer);
              resolve();
            }
          });
          worker.once('exit', (code) => reject(new Error(`the worker exited with ${code}`)));
        });

        browser = await chromium.launch({
          executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
          headless: true,
          args: [
            // Both flags, so nothing can ever capture the real screen or microphone.
            '--use-fake-ui-for-media-stream',
            '--use-fake-device-for-media-stream',
            `--use-file-for-fake-audio-capture=${questionWavs[serverName]}%noloop`,
            '--autoplay-policy=no-user-gesture-required',
          ],
        });
        const page = await (await browser.newContext()).newPage();
        const errors = [];
        page.on('pageerror', (e) => errors.push(String(e)));
        await page.goto(pageUrl);
        await page.waitForFunction(() => document.title === 'ready');

        const room = `browser-${serverName}-${Date.now()}`;
        const identity = 'browser-user';
        // The token's room configuration asks for the agent, which is the token-based dispatch.
        const token = mintToken({
          sub: identity,
          video: { roomJoin: true, room },
          roomConfig: { agents: [{ agentName }] },
        });
        await page.evaluate(([url, t]) => window.agentTest.connect(url, t), [`ws://127.0.0.1:${port}`, token]);
        const state = () => page.evaluate(() => window.agentTest.state());

        // Give the agent time to join, then speak.
        const deadline = Date.now() + 30000;
        while (true) {
          const participants = await page.evaluate(() => window.agentTest.state().connected);
          const status = await fetch(`http://127.0.0.1:${port}/twirp/livekit.RoomService/ListParticipants`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${mintToken({ sub: 'api', video: { roomAdmin: true, room } })}` },
            body: JSON.stringify({ room }),
          });
          const body = await status.json();
          if (participants && body.participants?.some((p) => p.kind === 'AGENT')) break;
          assert.ok(Date.now() < deadline, 'the agent did not join');
          await sleep(200);
        }
        await page.evaluate(() => window.agentTest.enableMic());

        // The reply comes through the browser's own WebRTC stack, and is decoded by its audio stack.
        const replyDeadline = Date.now() + 60000;
        let s = await state();
        while (!(s.firstHeardAt && Date.now() - s.lastHeardAt > 1500)) {
          assert.ok(Date.now() < replyDeadline, `no reply was heard: ${JSON.stringify(s)}`);
          await sleep(100);
          s = await state();
        }
        assert.deepEqual(errors, []);

        const { rate, data } = await page.evaluate(() => window.agentTest.heardPcm());
        const heard = await transcribe(to16k(Buffer.from(data, 'base64'), rate));
        const { country, capital } = QUESTIONS[serverName];
        assert.match(heard, capital, `Chrome heard: ${heard}`);

        const endToEnd = (s.firstHeardAt - s.micLastLoudAt) / 1000;
        const load = readFileSync('/proc/loadavg', 'utf8').split(' ')[0];
        console.log(`browser end to end on ${serverName}: ${endToEnd.toFixed(3)} s, load ${load}. Chrome heard: "${heard}"`);
        assert.ok(endToEnd > 0 && endToEnd < 30, `end to end ${endToEnd}`);

        // The transcripts, as the official client saw them. The agent's arrives with the reply.
        const seen = (await state()).transcripts;
        console.log(`transcripts on ${serverName}:`, JSON.stringify(seen));
        const agentLines = seen.filter((t) => t.identity !== identity);
        assert.ok(agentLines.some((t) => capital.test(t.text)), 'the agent transcript did not arrive');
        const userLines = seen.filter((t) => t.identity === identity);
        assert.ok(userLines.some((t) => t.text.toLowerCase().includes(country.toLowerCase())), `the user's transcript was not attributed to the user: ${JSON.stringify(seen)}`);
        assert.equal((await state()).agentAttributes['lk.agent.state'], 'listening');

        const records = readFileSync(metricsFile, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
        const turn = records.find((r) => r.type === 'turn');
        console.log(`agent's own latency on ${serverName}:`, JSON.stringify(turn.latency));
      } finally {
        await browser?.close();
        await stop(worker);
        await stop(server);
      }
    });
  }
});
