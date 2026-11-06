// Shared parts of the network suite: containers, packet impairment, browsers and measuring.
//
// The server runs in a container, with its traffic shaped by tc netem. Chrome runs on this machine and connects to the
// container's address. All of it is one machine, over a virtual bridge: this is emulation, and nothing it finds says
// anything final about real WiFi, real routers or a real second device. Every result is recorded with that attached.
//
// A browser here never captures the desktop: --use-fake-ui-for-media-stream is not used with real capture, and a screen
// share is stopped before it is published unless it turns out to be this browser's own tab.
import assert from 'node:assert/strict';
import { execFile, spawn, spawnSync } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs';
import { createRequire } from 'node:module';
import { loadavg } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { buildHarness } from '../interoperability/sdk-harness/build.mjs';
import { mintToken, startFixtureServer } from '../interoperability/sdk-harness/fixture.mjs';

export const here = dirname(fileURLToPath(import.meta.url));
export const repo = join(here, '../..');
// The tools live beside the repository, and a worktree sits a few levels inside it.
function findToolchain() {
  for (let dir = repo; dir !== dirname(dir); dir = dirname(dir)) {
    if (existsSync(join(dir, '.toolchain'))) return join(dir, '.toolchain');
  }
  return join(repo, '../.toolchain');
}
export const TOOLCHAIN = process.env.STREAMKIT_TOOLCHAIN ?? findToolchain();
const require = createRequire(join(here, '../interoperability/package.json'));
export const { chromium, firefox } = require('playwright-core');
export { mintToken };

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export const NET_IMAGE = 'elnova-nettest:1';
export const OURS_BINARY = process.env.STREAMKIT_STATIC_SERVER ?? join(repo, 'bin/streamkit-server-static');
export const REFERENCE_BINARY = process.env.REFERENCE_SERVER ?? join(TOOLCHAIN, '../reference/oracle/bin/x/livekit-server');

// ---- Docker ----

const OWNER_LABEL = 'elnova.nettest.owner';

function dockerRun(args, { input, allowFail = false, timeout = 60000 } = {}) {
  return new Promise((resolve, reject) => {
    const child = execFile('docker', args, { maxBuffer: 32 << 20, timeout }, (err, stdout, stderr) => {
      if (err && !allowFail) reject(new Error(`docker ${args.slice(0, 4).join(' ')}: ${stderr || err.message}`));
      else resolve(stdout.trim());
    });
    if (input !== undefined) child.stdin.end(input);
  });
}

let counter = 0;
const owned = new Set();
const ownedNetworks = new Set();
const name = (what) => `sk-nettest-${process.pid}-${what}-${++counter}`;

/** Removes what this process started, even when a test dies. */
function cleanUpNow() {
  for (const c of owned) spawnSync('docker', ['rm', '-f', c], { stdio: 'ignore' });
  for (const n of ownedNetworks) spawnSync('docker', ['network', 'rm', n], { stdio: 'ignore' });
  owned.clear();
  ownedNetworks.clear();
}
process.on('exit', cleanUpNow);
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    cleanUpNow();
    process.exit(1);
  });
}

/** Removes what an earlier run of this suite left behind when it was killed. Only its own label, and only dead owners. */
async function removeOrphans() {
  const out = await dockerRun(['ps', '-a', '--filter', `label=${OWNER_LABEL}`, '--format', `{{.Names}} {{.Label "${OWNER_LABEL}"}}`], { allowFail: true });
  for (const line of out.split('\n').filter(Boolean)) {
    const [container, pid] = line.split(' ');
    if (!existsSync(`/proc/${pid}`)) await dockerRun(['rm', '-f', container], { allowFail: true });
  }
  const nets = await dockerRun(['network', 'ls', '--filter', `label=${OWNER_LABEL}`, '--format', `{{.Name}} {{.Label "${OWNER_LABEL}"}}`], { allowFail: true });
  for (const line of nets.split('\n').filter(Boolean)) {
    const [net, pid] = line.split(' ');
    if (!existsSync(`/proc/${pid}`)) await dockerRun(['network', 'rm', net], { allowFail: true });
  }
}

export async function ensureImage(tag = NET_IMAGE, dockerfile = join(here, 'docker/Dockerfile')) {
  const found = await dockerRun(['image', 'inspect', '--format', 'ok', tag], { allowFail: true });
  if (found === 'ok') return;
  await dockerRun(['build', '-q', '-t', tag, '-f', dockerfile, dirname(dockerfile)], { timeout: 600000 });
}

/** A Docker network of its own for a test, so nothing here depends on the default bridge. */
export async function createNetwork(what = 'net') {
  const net = name(what);
  await dockerRun(['network', 'create', '--label', `${OWNER_LABEL}=${process.pid}`, net]);
  ownedNetworks.add(net);
  return {
    name: net,
    subnet: await dockerRun(['network', 'inspect', '--format', '{{(index .IPAM.Config 0).Subnet}}', net]),
    gateway: await dockerRun(['network', 'inspect', '--format', '{{(index .IPAM.Config 0).Gateway}}', net]),
    remove: async () => {
      await dockerRun(['network', 'rm', net], { allowFail: true });
      ownedNetworks.delete(net);
    },
  };
}

/** One container. */
export class Box {
  constructor(container) {
    this.container = container;
  }

  static async run({ what, image = NET_IMAGE, network, caps = ['NET_ADMIN'], memory = '2g', mounts = [], sysctls = [], extra = [], cmd = ['sleep', '86400'] }) {
    const container = name(what);
    owned.add(container);
    const args = ['run', '-d', '--name', container, '--label', `${OWNER_LABEL}=${process.pid}`, `--memory=${memory}`, `--memory-swap=${memory}`];
    if (network) args.push('--network', network);
    for (const c of caps) args.push('--cap-add', c);
    for (const [from, to] of mounts) args.push('-v', `${from}:${to}:ro`);
    for (const s of sysctls) args.push('--sysctl', s);
    args.push(...extra, image, ...cmd);
    await dockerRun(args);
    return new Box(container);
  }

  /** The address on a network, or on its only network. .NetworkSettings.IPAddress is gone in Docker 29. */
  async ip(network) {
    const format = network ? `{{(index .NetworkSettings.Networks "${network}").IPAddress}}` : '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}';
    return dockerRun(['inspect', '--format', format, this.container]);
  }

  sh(script, opts = {}) {
    return dockerRun(['exec', '-i', this.container, 'sh', '-c', script], opts);
  }

  /** Starts a long-running process in the container and returns straight away. */
  background(script) {
    return dockerRun(['exec', '-d', this.container, 'sh', '-c', script]);
  }

  async remove() {
    await dockerRun(['rm', '-f', this.container], { allowFail: true });
    owned.delete(this.container);
  }
}

// ---- The servers under test ----

export const SERVERS = (process.env.NET_SERVERS ?? 'reference,ours').split(',').filter(Boolean);
export const TURN_SECRET = 'network-suite-turn-secret-not-used-anywhere-else';

/**
 * Starts one server in a fresh container. kind is 'ours' or 'reference'. A fresh container each time means nothing
 * from the last test is left in the kernel or the process.
 */
export async function startServer(kind, { network, turn, what } = {}) {
  await ensureImage();
  const binary = kind === 'ours' ? OURS_BINARY : REFERENCE_BINARY;
  assert.ok(existsSync(binary), `no server binary at ${binary}. Build it: CGO_ENABLED=0 go build -o bin/streamkit-server-static ./server/cmd/streamkit-server`);
  const box = await Box.run({ what: what ?? kind, network, mounts: [[binary, '/server']] });
  const ip = await box.ip();
  assert.ok(ip, 'the container has no address');
  if (kind === 'ours') {
    const turnYaml = turn ? `\n  turn:\n    urls: ${JSON.stringify(turn.urls)}\n    secret: ${JSON.stringify(TURN_SECRET)}` : '';
    await box.sh('cat > /tmp/config.yaml', { input: `bind_address: 0.0.0.0\nrtc:\n  node_ip: ${ip}\n  udp_port: 7882\n  tcp_port: 7881${turnYaml}\n` });
    await box.background('/server --dev --config /tmp/config.yaml > /tmp/server.log 2>&1');
  } else {
    const body = turn ? `rtc:\n  turn_servers:\n${turn.servers.map((s) => `    - host: ${s.host}\n      port: ${s.port}\n      protocol: ${s.protocol}\n      secret: ${JSON.stringify(TURN_SECRET)}\n      ttl: 86400`).join('\n')}\n` : '';
    await box.sh('cat > /tmp/config.yaml', { input: body });
    await box.background(`/server --dev --bind 0.0.0.0 --node-ip ${ip} --config /tmp/config.yaml > /tmp/server.log 2>&1`);
  }
  const http = `http://${ip}:7880`;
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    try {
      up = (await fetch(`${http}/`, { signal: AbortSignal.timeout(1000) })).ok;
    } catch {
      await sleep(100);
    }
  }
  assert.ok(up, `${kind} server did not start: ${await box.sh('tail -20 /tmp/server.log', { allowFail: true })}`);
  return new Server(kind, box, ip);
}

/**
 * A running server, and the controls over its network. "down" is server to browser and "up" is browser to server.
 *
 * tc shapes what a device sends, so the upstream direction goes through an ifb device: traffic arriving on eth0 is
 * redirected to ifb0, and shaped as it leaves there, before the server sees it.
 *
 * With ports given, only those client ports are impaired. Both browsers are on the same host address, so that is the
 * only way to give one client a bad link and leave the other alone, as a viewer's bad WiFi would. Without ports, the
 * whole link is impaired, which is how the first hand measurements were made and is what a server's outage looks like.
 */
export class Server {
  constructor(kind, box, ip) {
    this.kind = kind;
    this.box = box;
    this.ip = ip;
    this.ws = `ws://${ip}:7880`;
    this.http = `http://${ip}:7880`;
    this.ifbReady = false;
    this.applied = null;
  }

  async stop() {
    await this.box.remove();
  }

  async logTail(lines = 40) {
    return this.box.sh(`tail -${lines} /tmp/server.log`, { allowFail: true });
  }

  async ensureIfb() {
    if (this.ifbReady) return;
    await this.box.sh(
      'ip link add ifb0 type ifb && ip link set ifb0 up && tc qdisc add dev eth0 handle ffff: ingress && ' +
        'tc filter add dev eth0 parent ffff: protocol ip u32 match u32 0 0 action mirred egress redirect dev ifb0',
    );
    this.ifbReady = true;
  }

  /** Applies impairment. down and up are netem arguments, such as "delay 20ms rate 1mbit limit 60". */
  async impair({ down, up, ports } = {}) {
    const lines = ['tc qdisc del dev eth0 root 2>/dev/null', 'tc qdisc del dev ifb0 root 2>/dev/null', 'true'];
    const apply = (dev, spec, match) => {
      if (!spec) return;
      if (!ports) {
        lines.push(`tc qdisc add dev ${dev} root netem ${spec}`);
        return;
      }
      lines.push(`tc qdisc add dev ${dev} root handle 1: prio bands 2 priomap ${Array(16).fill(0).join(' ')}`);
      lines.push(`tc qdisc add dev ${dev} parent 1:2 handle 20: netem ${spec}`);
      for (const p of ports) lines.push(`tc filter add dev ${dev} parent 1: protocol ip prio 1 u32 match ip ${match} ${p} 0xffff flowid 1:2`);
    };
    if (up) await this.ensureIfb();
    apply('eth0', down, 'dport');
    apply('ifb0', up, 'sport');
    this.applied = { down: down ?? null, up: up ?? null, ports: ports ?? 'all' };
    await this.box.sh(lines.join('\n'));
  }

  /** Lifts all impairment. */
  async clear() {
    this.applied = null;
    await this.box.sh('tc qdisc del dev eth0 root 2>/dev/null; tc qdisc del dev ifb0 root 2>/dev/null; true');
  }

  /** What netem has done so far, as proof it was really in the path: packets it passed on, and packets it dropped. */
  async netemCounters() {
    const out = await this.box.sh('tc -s qdisc show dev eth0; echo @@@; tc -s qdisc show dev ifb0 2>/dev/null; true');
    const [down, up] = out.split('@@@');
    const read = (text) => {
      const m = (text ?? '').match(/qdisc netem[\s\S]*?Sent (\d+) bytes (\d+) pkt \(dropped (\d+)/);
      return m ? { bytes: Number(m[1]), packets: Number(m[2]), dropped: Number(m[3]) } : null;
    };
    return { down: read(down), up: read(up) };
  }

  /** The client ports of established signalling connections. */
  async signalPorts() {
    const out = await this.box.sh("ss -Htn state established '( sport = :7880 )'");
    return out.split('\n').filter(Boolean).map((line) => Number(line.trim().split(/\s+/).at(-1).split(':').at(-1)));
  }

  /** Cuts signalling connections where they are, as a route going away would. Media is not touched. */
  async killSignalSockets() {
    await this.box.sh("ss -K state established '( sport = :7880 )' >/dev/null");
  }

  iptables(rules) {
    return this.box.sh(rules.map((r) => `iptables ${r}`).join('\n'));
  }
}

// ---- Browsers ----

let fixtureOnce;
export function fixture() {
  fixtureOnce ??= (async () => {
    await buildHarness();
    return startFixtureServer();
  })();
  return fixtureOnce;
}

export async function stopFixture() {
  if (fixtureOnce) (await fixtureOnce).server.close();
  fixtureOnce = undefined;
}

const CHROME_ARGS = ['--autoplay-policy=no-user-gesture-required', '--disable-background-timer-throttling', '--disable-renderer-backgrounding', '--disable-backgrounding-occluded-windows'];

export function launchChrome(extra = []) {
  return chromium.launch({ executablePath: process.env.CHROME ?? '/usr/bin/google-chrome', headless: true, args: [...CHROME_ARGS, ...extra] });
}

/** Chrome that can share one of its own tabs for real, and nothing else: there is no fake UI to pick a screen for it. */
export const launchSharingChrome = () => launchChrome(['--auto-select-tab-capture-source-by-title=NetworkSuiteSource']);

/** Chrome with fake cameras and microphones. Both flags together: getDisplayMedia then returns a fake screen too. */
export const launchFakeDeviceChrome = (extra = []) => launchChrome(['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', ...extra]);

export const FIREFOX_BINARY = process.env.FIREFOX ?? join(TOOLCHAIN, 'playwright/firefox-1555/firefox/firefox');

/**
 * Playwright's Firefox with fake cameras and microphones. getDisplayMedia is not called on it anywhere: unlike Chrome's
 * flags, these preferences give no promise about what it would capture.
 */
export function launchFirefox() {
  return firefox.launch({
    executablePath: FIREFOX_BINARY,
    headless: true,
    firefoxUserPrefs: {
      'media.navigator.streams.fake': true,
      'media.navigator.permission.disabled': true,
      'media.autoplay.default': 0,
      'media.autoplay.blocking_policy': 0,
      'media.peerconnection.ice.obfuscate_host_addresses': false,
    },
  });
}

// Runs in every page, before the SDK does.
const pageSetup = () => {
  const Native = window.RTCPeerConnection;
  window.__pcs = [];
  window.RTCPeerConnection = function (...args) {
    const pc = new Native(...args);
    window.__pcs.push(pc);
    return pc;
  };
  window.RTCPeerConnection.prototype = Native.prototype;

  // One reading of every connection this page has. Rows are keyed by connection, because a full reconnect starts counters again.
  window.__snapshot = async () => {
    const out = { at: Date.now(), rows: [], pairs: [], ports: [] };
    for (const [index, pc] of window.__pcs.entries()) {
      if (pc.connectionState === 'closed') continue;
      const report = await pc.getStats();
      const byId = new Map();
      // Which route is in use. Chrome can have several nominated at once, and names the real one on its transport.
      const selected = new Set();
      report.forEach((s) => {
        byId.set(s.id, s);
        if (s.type === 'transport' && s.selectedCandidatePairId) selected.add(s.selectedCandidatePairId);
      });
      report.forEach((s) => {
        if (s.type === 'inbound-rtp' || s.type === 'outbound-rtp') {
          const codec = byId.get(s.codecId);
          out.rows.push({
            key: `${index}/${s.id}`,
            type: s.type,
            kind: s.kind,
            rid: s.rid,
            mime: codec?.mimeType,
            framesDecoded: s.framesDecoded,
            framesEncoded: s.framesEncoded,
            frameWidth: s.frameWidth,
            frameHeight: s.frameHeight,
            framesPerSecond: s.framesPerSecond,
            bytes: s.bytesReceived ?? s.bytesSent,
            packets: s.packetsReceived ?? s.packetsSent,
            lost: s.packetsLost,
            retransmitted: s.retransmittedPacketsReceived ?? s.retransmittedPacketsSent,
            freezes: s.freezeCount,
            freezeSeconds: s.totalFreezesDuration,
            nacks: s.nackCount,
            plis: s.pliCount,
            firs: s.firCount,
            jitterBufferDelay: s.jitterBufferDelay,
            jitterBufferEmitted: s.jitterBufferEmittedCount,
            samples: s.totalSamplesReceived,
            concealed: s.concealedSamples,
            audioLevel: s.audioLevel,
            audioEnergy: s.totalAudioEnergy,
            targetBitrate: s.targetBitrate,
            limitedBy: s.qualityLimitationReason,
            encoder: s.encoderImplementation,
            decoder: s.decoderImplementation,
          });
        }
        if (s.type === 'local-candidate' && s.port) out.ports.push({ port: s.port, protocol: s.protocol, candidateType: s.candidateType, relayProtocol: s.relayProtocol });
        if (s.type === 'candidate-pair' && (selected.has(s.id) || s.selected)) {
          const local = byId.get(s.localCandidateId);
          const remote = byId.get(s.remoteCandidateId);
          out.pairs.push({
            pc: index,
            rtt: s.currentRoundTripTime,
            available: s.availableIncomingBitrate ?? s.availableOutgoingBitrate,
            localType: local?.candidateType,
            localProtocol: local?.protocol,
            localPort: local?.port,
            localAddress: local?.address,
            relayProtocol: local?.relayProtocol,
            remoteType: remote?.candidateType,
            remoteProtocol: remote?.protocol,
            remoteAddress: remote?.address,
          });
        }
      });
    }
    return out;
  };

  // Events with wall-clock times, so they can be put on the same axis as frames.
  window.__events = [];
  window.__listen = () => {
    const room = window.harness.room;
    for (const name of ['reconnecting', 'reconnected', 'signalReconnecting', 'signalConnected', 'disconnected', 'connectionStateChanged', 'trackSubscribed', 'trackUnsubscribed', 'trackStreamStateChanged', 'participantConnected', 'participantDisconnected']) {
      room.on(name, (...args) => window.__events.push({ at: Date.now(), name, detail: args.map((a) => (typeof a === 'object' ? (a?.identity ?? a?.trackSid ?? undefined) : a)) }));
    }
  };
};

// The barcode's size and position as fractions of the frame, so it reads the same from any layer.
const BITS = 32;
const BLOCK_X = 56 / 1920;
const BLOCK_Y = 56 / 1080;

// Runs in the viewer. Reads the clock barcode from every frame it presents, on every video that appears.
const viewerSetup = ({ BITS, BLOCK_X, BLOCK_Y }) => {
  window.__frames = [];
  const W = 640;
  const H = 360;
  const canvas = document.createElement('canvas');
  canvas.width = W;
  canvas.height = H;
  const g = canvas.getContext('2d', { willReadFrequently: true });
  const watch = (video) => {
    video.style.cssText = 'width:1000px;height:562px;display:block';
    const source = video.dataset.source;
    const onFrame = (_, meta) => {
      const at = Date.now();
      g.drawImage(video, 0, 0, W, H);
      const row = g.getImageData(0, Math.round((BLOCK_Y * H) / 6), W, 1).data;
      let word = 0;
      for (let i = 0; i < BITS; i++) {
        const x = Math.round((i + 0.5) * BLOCK_X * W);
        if (row[x * 4] > 128) word += 2 ** i;
      }
      const value = word % 2 ** 28;
      let ones = 0;
      for (let i = 0; i < 28; i++) ones += (value >>> i) & 1;
      const good = Math.floor(word / 2 ** 28) === ones % 16;
      window.__frames.push({ at, latency: good ? ((at & 0x0fffffff) - value + 2 ** 28) % 2 ** 28 : null, width: meta.width, height: meta.height, source });
      if (video.isConnected) video.requestVideoFrameCallback(onFrame);
    };
    video.requestVideoFrameCallback(onFrame);
  };
  setInterval(() => {
    document.querySelectorAll('video').forEach((v) => {
      if (!v.__watched) {
        v.__watched = true;
        watch(v);
      }
    });
  }, 50);
};

// Runs in the publisher, on any source: stamps the clock into the top rows of a 2D canvas, or of a page.
const barcodeWord = () => {
  const value = Date.now() & 0x0fffffff;
  let ones = 0;
  for (let i = 0; i < 28; i++) ones += (value >>> i) & 1;
  return (value | (ones << 28)) >>> 0;
};

const shareTabSetup = ({ barcode, BITS }) => {
  const word = new Function(`return (${barcode})`)();
  const BLOCK = 56;
  document.title = 'NetworkSuiteSource';
  document.body.style.cssText = 'margin:0;background:#fff;color:#111;overflow:hidden;font:12px/16px monospace';
  const bar = document.createElement('canvas');
  bar.width = BITS * BLOCK;
  bar.height = BLOCK;
  bar.style.cssText = 'position:fixed;left:0;top:0;z-index:2';
  document.body.appendChild(bar);
  const text = document.createElement('div');
  text.style.cssText = `position:absolute;left:0;top:${BLOCK}px;width:1920px;white-space:pre-wrap`;
  const line = 'const screen = shared.withSmallText(everyCharacter, mustStayReadable); // the quick brown fox jumps over the lazy dog 0123456789 ';
  text.textContent = Array.from({ length: 400 }, (_, i) => `${String(i).padStart(4, '0')} ${line}${line}`).join('\n');
  document.body.appendChild(text);
  const g = bar.getContext('2d');
  const draw = () => {
    const w = word();
    for (let i = 0; i < BITS; i++) {
      g.fillStyle = (w >>> i) & 1 ? '#fff' : '#000';
      g.fillRect(i * BLOCK, 0, BLOCK, BLOCK);
    }
  };
  draw();
  setInterval(draw, 16);
};

// A stand-in for a camera: always moving, about as hard to compress as a person talking in a busy room, with the barcode on
// top. Harder than this and Chrome drops frames from its lower layers to keep to their bitrates, which says nothing about
// the network. It needs no capture device at all. Sound is a tone from WebAudio.
const makeCameraSource = ({ barcode, BITS, BLOCK_X, BLOCK_Y, width, height, fps, tiles }) => {
  const word = new Function(`return (${barcode})`)();
  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const g = canvas.getContext('2d', { alpha: false });
  const cols = 24;
  const rows = 14;
  const cellW = Math.ceil(width / cols);
  const cellH = Math.ceil(height / rows);
  let n = 0;
  const draw = () => {
    n++;
    for (let i = 0; i < tiles; i++) {
      const c = Math.floor(Math.random() * cols);
      const r = 1 + Math.floor(Math.random() * rows);
      g.fillStyle = `hsl(${Math.floor(Math.random() * 360)},70%,${30 + Math.floor(Math.random() * 40)}%)`;
      g.fillRect(c * cellW, r * cellH, cellW, cellH);
    }
    g.fillStyle = '#222';
    g.fillRect((n * 7) % width, height / 2, 120, 120);
    const w = word();
    const bw = BLOCK_X * width;
    const bh = BLOCK_Y * height;
    for (let i = 0; i < BITS; i++) {
      g.fillStyle = (w >>> i) & 1 ? '#fff' : '#000';
      g.fillRect(Math.floor(i * bw), 0, Math.ceil(bw), Math.ceil(bh));
    }
  };
  draw();
  setInterval(draw, 1000 / fps);
  const video = canvas.captureStream(fps).getVideoTracks()[0];
  const audioContext = new AudioContext();
  const tone = audioContext.createOscillator();
  tone.frequency.value = 440;
  const dest = audioContext.createMediaStreamDestination();
  tone.connect(dest);
  tone.start();
  audioContext.resume();
  window.__sources = { video, audio: dest.stream.getAudioTracks()[0], audioContext };
};

export const SCREEN_PUBLISH = { simulcast: true, screenShareEncoding: { maxBitrate: 5_000_000, maxFramerate: 30 } };
export const SCREEN_CAPTURE = { video: { frameRate: 30, width: 1920, height: 1080 }, contentHint: 'detail' };

/**
 * One client in a page. Takes a reading of its connections every second for as long as it lives, whatever the test is
 * doing, so a test only has to say which stretch of time it wants to know about.
 */
export class Client {
  constructor({ page, browser, ownsBrowser, identity }) {
    this.page = page;
    this.browser = browser;
    this.ownsBrowser = ownsBrowser;
    this.identity = identity;
    this.readings = [];
    this.errors = [];
    this.closed = false;
  }

  startReading() {
    this.loop = (async () => {
      while (!this.closed) {
        const next = Date.now() + 1000;
        try {
          this.readings.push(await this.page.evaluate(() => window.__snapshot()));
        } catch (e) {
          if (!this.closed) this.errors.push(`reading: ${e.message.split('\n')[0]}`);
        }
        await sleep(Math.max(50, next - Date.now()));
      }
    })();
  }

  /** The UDP ports this client could be using. Chrome has one per local address, and can move between them. */
  async mediaPorts() {
    const { ports } = await this.page.evaluate(() => window.__snapshot());
    return [...new Set(ports.filter((p) => p.protocol === 'udp').map((p) => p.port))];
  }

  async frames() {
    return this.page.evaluate(() => window.__frames ?? []);
  }

  async events() {
    return this.page.evaluate(() => window.__events ?? []);
  }

  state() {
    return this.page.evaluate(() => window.harness.connectionState());
  }

  async close() {
    this.closed = true;
    await this.loop?.catch(() => {});
    if (this.ownsBrowser) await this.browser.close().catch(() => {});
    else await this.page.context().close().catch(() => {});
  }
}

const pageFor = { streamkit: 'streamkit.html', official: 'official.html' };

/** Opens one connected client. It publishes nothing yet. */
export async function openClient(browser, { identity, room, ws, sdk = process.env.NET_SDK ?? 'streamkit', viewport = { width: 1280, height: 720 }, ownsBrowser = true, query = {}, permissions = [], connectTimeout = 45000, origin }) {
  const { port } = await fixture();
  const ctx = await browser.newContext({ viewport, permissions });
  await ctx.addInitScript(pageSetup);
  const page = await ctx.newPage();
  const client = new Client({ page, browser, ownsBrowser, identity });
  page.on('pageerror', (e) => client.errors.push(e.message));
  const params = new URLSearchParams({ ws, identity, room, publish: '0', mode: 'dual', adaptive: '1', ...query });
  await page.goto(`${origin ?? `http://127.0.0.1:${port}`}/${pageFor[sdk]}?${params}`);
  await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: connectTimeout });
  const state = await page.$eval('#state', (e) => `${e.dataset.state} ${e.textContent}`);
  assert.ok(state.startsWith('connected'), `${identity} failed to connect: ${state}`);
  await page.evaluate(() => window.__listen());
  client.startReading();
  return client;
}

/** Turns a viewer's page into a measuring instrument. */
export async function measureFrames(client) {
  await client.page.evaluate(viewerSetup, { BITS, BLOCK_X, BLOCK_Y });
}

/** Shares this browser's own tab, with real capture. Stops before publishing if it is anything else. */
export async function publishScreen(client, { capture = SCREEN_CAPTURE, publish = SCREEN_PUBLISH } = {}) {
  await client.page.evaluate(shareTabSetup, { barcode: barcodeWord.toString(), BITS });
  await client.page.evaluate(
    async ({ capture, publish }) => {
      const stream = await navigator.mediaDevices.getDisplayMedia({ video: capture?.video ?? true, audio: false, preferCurrentTab: true, selfBrowserSurface: 'include' });
      const [track] = stream.getVideoTracks();
      const surface = track.getSettings().displaySurface;
      if (surface !== 'browser') {
        track.stop();
        throw new Error(`refusing to share a ${surface}: only this tab may be captured`);
      }
      if (capture?.contentHint) track.contentHint = capture.contentHint;
      await window.harness.room.localParticipant.publishTrack(track, { source: 'screen_share', name: 'screen', ...publish });
    },
    { capture, publish },
  );
}

/** Publishes a moving picture and a tone as a camera and a microphone. */
export async function publishCamera(client, { width = 1280, height = 720, fps = 30, publish = {}, audio = true, tiles = 5 } = {}) {
  await client.page.evaluate(makeCameraSource, { barcode: barcodeWord.toString(), BITS, BLOCK_X, BLOCK_Y, width, height, fps, tiles });
  await client.page.evaluate(
    async ({ publish, audio }) => {
      const { video, audio: mic } = window.__sources;
      await window.harness.room.localParticipant.publishTrack(video, { source: 'camera', name: 'camera', simulcast: true, ...publish });
      if (audio) await window.harness.room.localParticipant.publishTrack(mic, { source: 'microphone', name: 'microphone' });
    },
    { publish, audio },
  );
}

// ---- Measuring ----

function percentile(sorted, p) {
  return sorted.length === 0 ? null : sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))];
}

const round = (v, places = 0) => (v === null || v === undefined || Number.isNaN(v) ? null : Math.round(v * 10 ** places) / 10 ** places);
const mean = (list) => (list.length === 0 ? null : list.reduce((a, b) => a + b, 0) / list.length);

/** Adds up what changed between readings, for rows of one kind, within a stretch of time. */
function deltas(readings, from, to, predicate) {
  const totals = { packets: 0, bytes: 0, lost: 0, retransmitted: 0, freezes: 0, freezeSeconds: 0, nacks: 0, plis: 0, firs: 0, frames: 0, samples: 0, concealed: 0 };
  const last = new Map();
  let first = null;
  let end = null;
  for (const reading of readings) {
    for (const row of reading.rows) {
      if (!predicate(row)) continue;
      const before = last.get(row.key);
      last.set(row.key, row);
      if (reading.at < from || reading.at > to || !before) continue;
      first ??= reading.at;
      end = reading.at;
      const add = (out, field) => (totals[out] += Math.max(0, (row[field] ?? 0) - (before[field] ?? 0)));
      add('packets', 'packets');
      add('bytes', 'bytes');
      add('lost', 'lost');
      add('retransmitted', 'retransmitted');
      add('freezes', 'freezes');
      add('freezeSeconds', 'freezeSeconds');
      add('nacks', 'nacks');
      add('plis', 'plis');
      add('firs', 'firs');
      add('frames', 'framesDecoded');
      add('samples', 'samples');
      add('concealed', 'concealed');
    }
  }
  totals.seconds = first === null || end === first ? 0 : (end - first) / 1000 + 1;
  return totals;
}

/**
 * Everything a viewer saw in a stretch of time. All times are wall-clock milliseconds, and both browsers share a clock,
 * so latency is from drawing at the publisher to presenting at the viewer.
 */
export async function viewerSummary(client, from, to = Date.now()) {
  const frames = (await client.frames()).filter((f) => f.at >= from && f.at <= to);
  const latencies = frames.map((f) => f.latency).filter((l) => l !== null).sort((a, b) => a - b);
  const gaps = frames.slice(1).map((f, i) => f.at - frames[i].at);
  // The time up to the first frame and after the last one counts too: a window with no frames at all is one long gap.
  const edges = frames.length ? [frames[0].at - from, to - frames.at(-1).at] : [to - from];
  const widths = {};
  for (const f of frames) widths[f.width] = (widths[f.width] ?? 0) + 1;
  const video = deltas(client.readings, from, to, (r) => r.type === 'inbound-rtp' && r.kind === 'video');
  const audio = deltas(client.readings, from, to, (r) => r.type === 'inbound-rtp' && r.kind === 'audio');
  const readings = client.readings.filter((r) => r.at >= from && r.at <= to);
  const rtts = readings.flatMap((r) => r.pairs.map((p) => p.rtt)).filter((v) => typeof v === 'number');
  const seconds = (to - from) / 1000;
  const expected = video.packets + video.lost;
  return {
    seconds: round(seconds, 1),
    fps: round(frames.length / Math.max(seconds, 0.001), 1),
    decodedFps: video.seconds ? round(video.frames / video.seconds, 1) : 0,
    p50: percentile(latencies, 0.5),
    p90: percentile(latencies, 0.9),
    p99: percentile(latencies, 0.99),
    readable: `${latencies.length}/${frames.length}`,
    longestGapMs: Math.max(0, ...gaps, ...edges),
    gapsOver1s: gaps.filter((g) => g > 1000).length,
    freezes: video.freezes,
    freezeMs: round(video.freezeSeconds * 1000),
    widths,
    topWidth: Object.keys(widths).length ? Math.max(...Object.keys(widths).map(Number)) : null,
    usualWidth: Object.keys(widths).length ? Number(Object.entries(widths).sort((a, b) => b[1] - a[1])[0][0]) : null,
    lastWidth: frames.at(-1)?.width ?? null,
    kbps: video.seconds ? round((video.bytes * 8) / video.seconds / 1000) : 0,
    nacks: video.nacks,
    plis: video.plis,
    firs: video.firs,
    packets: video.packets,
    lost: video.lost,
    residualLossPct: expected ? round((video.lost / expected) * 100, 2) : null,
    retransmitted: video.retransmitted,
    rttMs: rtts.length ? round(mean(rtts) * 1000) : null,
    audioConcealedPct: audio.samples ? round((audio.concealed / audio.samples) * 100, 1) : null,
  };
}

/** What a publisher is sending, per layer, over a stretch of time. */
export function senderSummary(client, from, to = Date.now()) {
  const out = {};
  const before = new Map();
  for (const reading of client.readings) {
    for (const row of reading.rows) {
      if (row.type !== 'outbound-rtp' || row.kind !== 'video') continue;
      const prev = before.get(row.key);
      before.set(row.key, row);
      if (!prev || reading.at < from || reading.at > to) continue;
      const o = (out[row.rid ?? 'only'] ??= { width: 0, bytes: 0, frames: 0, n: 0, limitedBy: {}, encoder: '' });
      o.width = row.frameWidth ?? o.width;
      o.bytes += Math.max(0, (row.bytes ?? 0) - (prev.bytes ?? 0));
      o.frames += Math.max(0, (row.framesEncoded ?? 0) - (prev.framesEncoded ?? 0));
      o.n++;
      o.encoder = row.encoder ?? o.encoder;
      const reason = row.limitedBy ?? 'none';
      o.limitedBy[reason] = (o.limitedBy[reason] ?? 0) + 1;
    }
  }
  for (const o of Object.values(out)) {
    o.kbps = o.n ? round((o.bytes * 8) / o.n / 1000) : 0;
    o.fps = o.n ? round(o.frames / o.n, 1) : 0;
    delete o.bytes;
    delete o.frames;
    delete o.n;
  }
  return out;
}

/** The path media takes now: what kind of route, over what. */
export async function selectedPath(client) {
  const { pairs } = await client.page.evaluate(() => window.__snapshot());
  return pairs;
}

/** How long until a viewer presents a frame after a moment, or null if it never does before the deadline. */
export async function waitForFrameAfter(client, moment, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const hit = await client.page.evaluate((m) => window.__frames?.find((f) => f.at >= m)?.at ?? null, moment);
    if (hit !== null) return hit - moment;
    await sleep(100);
  }
  return null;
}

/** The first time frames come steadily after a moment: a frame every half second for a few seconds, not one lucky frame. */
export async function waitForSteadyFramesAfter(client, moment, timeoutMs, { minFps = 5, forMs = 3000 } = {}) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const frames = await client.page.evaluate((m) => (window.__frames ?? []).filter((f) => f.at >= m).map((f) => f.at), moment);
    for (let i = 0; i < frames.length; i++) {
      const end = frames[i] + forMs;
      if (end > Date.now()) break;
      const inside = frames.filter((t) => t >= frames[i] && t < end);
      if (inside.length >= (minFps * forMs) / 1000) return frames[i] - moment;
    }
    await sleep(250);
  }
  return null;
}

/** Waits until most recent frames are at least this wide. Returns how long that took, or null. */
export async function waitForWidth(client, moment, width, timeoutMs, { holdMs = 3000 } = {}) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const frames = await client.page.evaluate((m) => (window.__frames ?? []).filter((f) => f.at >= m).map((f) => [f.at, f.width]), moment);
    const first = frames.findIndex(([at, w], i) => w >= width && frames.slice(i).every(([a, wide]) => wide >= width || a > at + holdMs));
    if (first >= 0 && frames.at(-1)[0] - frames[first][0] >= holdMs) return frames[first][0] - moment;
    await sleep(500);
  }
  return null;
}

// ---- Results ----

const resultsDir = join(here, 'results');
export const RUN_ID = process.env.NET_RUN_ID ?? new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-');

export function machineLoad() {
  const [one, five] = loadavg();
  const mem = readFileSync('/proc/meminfo', 'utf8');
  const available = Number(mem.match(/MemAvailable:\s+(\d+)/)[1]);
  return { load1: round(one, 1), load5: round(five, 1), memAvailableGb: round(available / 1048576, 1), busyPct: lastBusyPct };
}

let lastBusyPct = null;

function cpuTimes() {
  const [, ...fields] = readFileSync('/proc/stat', 'utf8').split('\n')[0].trim().split(/\s+/);
  const values = fields.map(Number);
  return { idle: values[3] + values[4], total: values.reduce((a, b) => a + b, 0) };
}

/**
 * Waits for the machine to have room before a measurement starts. Encoders starved of CPU drop frames, and a test run then
 * would measure the other work on this machine and not the network. Other sessions share it, so this waits a while for them,
 * then goes ahead anyway and records how busy the machine was, so a reader can discount the row.
 */
export async function waitForRoom({ busyBelowPct = 40, patienceMs = Number(process.env.NET_ROOM_PATIENCE_MS ?? 240000) } = {}) {
  const deadline = Date.now() + patienceMs;
  for (;;) {
    const before = cpuTimes();
    await sleep(2000);
    const after = cpuTimes();
    lastBusyPct = round((1 - (after.idle - before.idle) / Math.max(1, after.total - before.total)) * 100);
    if (lastBusyPct < busyBelowPct || Date.now() > deadline) return lastBusyPct;
    await sleep(8000);
  }
}

/** Adds one measured row to this run's results. Nothing else writes numbers into the report. */
export function recordResult(row) {
  mkdirSync(resultsDir, { recursive: true });
  appendFileSync(join(resultsDir, `${RUN_ID}.jsonl`), `${JSON.stringify({ at: new Date().toISOString(), ...row })}\n`);
}

export function readResults(run) {
  const files = readdirSync(resultsDir).filter((f) => f.endsWith('.jsonl'));
  const chosen = run ? [`${run}.jsonl`] : files;
  return chosen.flatMap((f) => readFileSync(join(resultsDir, f), 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)));
}

/**
 * Checks that are collected, not thrown, so that a failing check still records the numbers. Thresholds come from what a
 * person would accept and from what the reference achieves, and are written at the top of each test file.
 */
export class Checks {
  constructor() {
    this.failed = [];
    this.passed = [];
  }

  check(ok, message) {
    (ok ? this.passed : this.failed).push(message);
    return ok;
  }
}

// ---- Running a scenario on both servers ----

let roomCounter = 0;
export const nextRoom = (what = 'call') => `${what}-${process.pid}-${++roomCounter}`;

/**
 * A viewer and a sharer in one room, neither publishing yet. The viewer's ports are known, so it can be impaired alone.
 *
 * Adaptive stream is off unless asked for. With it on, a viewer's small element limits it to a lower layer whatever the
 * link can do, and the two servers read the same element size differently: for a 1000 px element, the reference picks the
 * 960 px layer and ours the 1920 px one. That difference is real, but it would hide what the network does.
 */
export async function openCall(server, { sdk, adaptive = false, mode = 'dual' } = {}) {
  const room = nextRoom();
  const viewer = await openClient(await launchChrome(), { identity: 'viewer', room, ws: server.ws, sdk, query: { adaptive: adaptive ? '1' : '0', mode } });
  await measureFrames(viewer);
  const viewerPorts = [...(await viewer.mediaPorts()), ...(await server.signalPorts())];
  const publisher = await openClient(await launchSharingChrome(), { identity: 'sharer', room, ws: server.ws, sdk, viewport: { width: 1920, height: 1080 }, query: { mode } });
  return {
    room,
    viewer,
    publisher,
    viewerPorts,
    close: async () => {
      await viewer.close();
      await publisher.close();
    },
  };
}

/**
 * One test case on each server. The reference goes first and is only recorded: judge runs on both, but only a failure
 * on ours fails the test. If the reference misses a threshold too, that is written down beside the number, because it
 * says something about the threshold. A person reading the results decides what that means.
 *
 * run(server) returns the measurements. link says what was done to the network, in words, for the results.
 */
export function onBothServers(describe, it, title, { link, run, judge, timeout = 240000, sdk }) {
  describe(title, () => {
    let reference;
    for (const kind of SERVERS) {
      it(kind, { timeout: timeout + 240000 }, async (t) => {
        await waitForRoom();
        const started = machineLoad();
        const network = await createNetwork();
        const server = await startServer(kind, { network: network.name });
        let metrics;
        let error;
        try {
          metrics = await run(server, { reference });
        } catch (e) {
          error = e;
          t.diagnostic(`server log tail:\n${await server.logTail(15)}`);
        } finally {
          await server.stop();
          await network.remove();
        }
        const checks = new Checks();
        if (metrics && judge) judge(metrics, checks, reference);
        const row = {
          scenario: title,
          server: kind,
          sdk: sdk ?? process.env.NET_SDK ?? 'streamkit',
          link,
          network: 'emulated: tc netem on a Docker bridge, one machine',
          load: { started, ended: machineLoad() },
          metrics,
          failed: checks.failed,
          passed: checks.passed,
          error: error ? String(error.message ?? error).split('\n')[0] : undefined,
        };
        recordResult(row);
        t.diagnostic(`${kind}: ${JSON.stringify(metrics)} load=${started.load1}`);
        if (checks.failed.length) t.diagnostic(`${kind} missed: ${checks.failed.join('; ')}`);
        if (kind === 'reference') {
          reference = metrics;
          if (error) t.diagnostic(`reference failed: ${error.message}`);
          return;
        }
        if (error) throw error;
        assert.deepEqual(checks.failed, [], `${title} on ours`);
      });
    }
  });
}

await removeOrphans();
