// Test client: our own SDK, driven by URL parameters and from the test through window.harness. The surface
// matches the official client's harness in oracle/client.ts, so that one test can drive either.
import {
  ConnectionError,
  ConnectionQuality,
  ConnectionState,
  DataPacketKind,
  Room,
  RoomEvent,
  Track,
  TrackSource,
  VideoQuality,
  createLocalVideoTrack,
  setLogLevel,
} from '@elnova-streamkit/client';

const q = new URLSearchParams(location.search);
const state = document.getElementById('state')!;
const media = document.getElementById('media')!;
const events: Array<{ t: number; name: string; detail?: unknown }> = [];
const started = performance.now();
if (q.get('debug')) setLogLevel('debug');

function record(name: string, detail?: unknown) {
  events.push({ t: Math.round(performance.now() - started), name, detail });
}

const room = new Room({
  adaptiveStream: q.get('adaptive') === '1',
  dynacast: q.get('dynacast') !== '0',
  publishDefaults: q.get('codec') ? { videoCodec: q.get('codec') as any } : undefined,
});

for (const name of Object.values(RoomEvent)) {
  room.on(name as any, (...args: unknown[]) => {
    const summary = args.map((a: any) => a?.identity ?? a?.trackSid ?? a?.sid ?? (typeof a === 'object' ? undefined : a));
    record(name as string, summary);
  });
}

const received: Array<{ text: string; from?: string; kind?: number; topic?: string }> = [];
room.on(RoomEvent.DataReceived, (payload, participant, kind, topic) => {
  received.push({ text: new TextDecoder().decode(payload), from: participant?.identity, kind, topic });
});

let disconnectReason: unknown;
room.on(RoomEvent.Disconnected, (reason) => {
  disconnectReason = reason;
});

const autoAttach = q.get('attach') !== '0';
room.on(RoomEvent.TrackSubscribed, (track) => {
  if (!autoAttach) return;
  const el = track.attach();
  // Silent, so a test browser's autoplay rules have no say, except in the test that is about them.
  if (q.get('unmuted') !== '1' || track.kind === 'video') el.muted = true;
  el.dataset.source = track.source;
  el.dataset.kind = track.kind;
  el.dataset.sid = track.sid ?? '';
  media.appendChild(el);
});

room.on(RoomEvent.TrackUnsubscribed, (_track, publication) => {
  document.querySelectorAll(`[data-sid="${publication.trackSid}"]`).forEach((el) => el.remove());
});

async function subscribedStats() {
  const out: unknown[] = [];
  for (const p of room.remoteParticipants.values()) {
    for (const pub of p.trackPublications.values()) {
      const t = pub.track as any;
      if (!t) continue;
      const report = await t.getRTCStatsReport();
      const inbound: any[] = [];
      report?.forEach((s: any) => {
        if (s.type === 'inbound-rtp') inbound.push(s);
      });
      const r = inbound[0] ?? {};
      out.push({
        identity: p.identity,
        kind: t.kind,
        source: t.source,
        sid: pub.trackSid,
        packetsReceived: r.packetsReceived ?? 0,
        bytesReceived: r.bytesReceived ?? 0,
        framesDecoded: r.framesDecoded ?? 0,
        audioLevel: r.audioLevel ?? null,
        totalSamplesReceived: r.totalSamplesReceived ?? 0,
        width: r.frameWidth ?? null,
        height: r.frameHeight ?? null,
        codec: r.codecId ?? null,
      });
    }
  }
  return out;
}

// What the browser is really sending for a local track, one row per encoding.
async function outboundStats(source: string) {
  const pub = room.localParticipant.getTrackPublication(source as TrackSource);
  const t = pub?.track as any;
  if (!t?.sender) return [];
  const report: RTCStatsReport = await t.sender.getStats();
  const codecs = new Map<string, any>();
  report.forEach((s: any) => {
    if (s.type === 'codec') codecs.set(s.id, s);
  });
  const rows: any[] = [];
  report.forEach((s: any) => {
    if (s.type === 'outbound-rtp') {
      rows.push({
        rid: s.rid ?? '',
        active: s.active,
        bytesSent: s.bytesSent ?? 0,
        framesSent: s.framesSent ?? 0,
        width: s.frameWidth ?? 0,
        height: s.frameHeight ?? 0,
        mimeType: codecs.get(s.codecId)?.mimeType ?? '',
      });
    }
  });
  return rows;
}

function encodingStates(source: string) {
  const pub = room.localParticipant.getTrackPublication(source as TrackSource);
  const sender = (pub?.track as any)?.sender as RTCRtpSender | undefined;
  return sender ? sender.getParameters().encodings.map((e) => ({ rid: e.rid ?? '', active: e.active, maxBitrate: e.maxBitrate })) : [];
}

function frameHash(sid?: string) {
  const v = document.querySelector<HTMLVideoElement>(sid ? `video[data-sid="${sid}"]` : 'video[data-kind="video"]');
  if (!v || !v.videoWidth) return null;
  const c = document.createElement('canvas');
  c.width = 64;
  c.height = 36;
  const g = c.getContext('2d', { willReadFrequently: true })!;
  g.drawImage(v, 0, 0, c.width, c.height);
  const d = g.getImageData(0, 0, c.width, c.height).data;
  let h = 2166136261;
  for (let i = 0; i < d.length; i += 4) h = Math.imul(h ^ d[i] ^ d[i + 1] ^ d[i + 2], 16777619);
  return { hash: h >>> 0, width: v.videoWidth, height: v.videoHeight };
}

// A track of known content, to prove what the far end decodes is what was sent.
function canvasTrack(rgb: [number, number, number]) {
  const canvas = document.createElement('canvas');
  canvas.width = 320;
  canvas.height = 240;
  const g = canvas.getContext('2d')!;
  let n = 0;
  setInterval(() => {
    n++;
    g.fillStyle = `rgb(${rgb[0]},${rgb[1]},${rgb[2]})`;
    g.fillRect(0, 0, 320, 240);
    g.fillStyle = 'white';
    g.fillRect(n % 300, 100, 20, 20);
  }, 33);
  return canvas.captureStream(30).getVideoTracks()[0];
}

function centerColour(sid: string) {
  const v = document.querySelector<HTMLVideoElement>(`video[data-sid="${sid}"]`);
  if (!v || !v.videoWidth) return null;
  const c = document.createElement('canvas');
  c.width = 4;
  c.height = 4;
  const g = c.getContext('2d', { willReadFrequently: true })!;
  g.drawImage(v, 0, 0, 4, 4);
  const d = g.getImageData(0, 0, 1, 1).data;
  return [d[0], d[1], d[2]];
}

(window as any).harness = {
  sdk: 'streamkit',
  room,
  events,
  subscribedStats,
  outboundStats,
  encodingStates,
  frameHash,
  centerColour,
  canvasTrack,
  received,
  disconnectReason: () => disconnectReason,
  Track,
  Room,
  TrackSource,
  VideoQuality,
  DataPacket_Kind: DataPacketKind,
  ConnectionQuality,
  ConnectionError,
  createLocalVideoTrack,
  eventCount: (name: string) => events.filter((e) => e.name === name).length,
  publication: (identity: string, source: string) => {
    const p = room.remoteParticipants.get(identity);
    return p && [...p.trackPublications.values()].find((x) => x.source === source);
  },
  subscribedSources: (identity: string) => {
    const p = room.remoteParticipants.get(identity);
    return p ? [...p.trackPublications.values()].filter((x) => x.isSubscribed && x.track).map((x) => x.source) : [];
  },
  connectionState: () => room.state,
  remoteIdentities: () => [...room.remoteParticipants.keys()],
  localSid: () => room.localParticipant.sid,
  token: () => room.engine?.token,
  rtt: () => room.engine?.signal.rtt,
  serverVersion: () => room.serverInfo?.version,
};

async function main() {
  if (q.get('autoconnect') === '0') {
    state.dataset.state = 'idle';
    return;
  }
  const identity = q.get('identity') ?? 'anon';
  const roomName = q.get('room') ?? 'oracle';
  const params = new URLSearchParams({ identity, room: roomName });
  if (q.get('grants')) params.set('grants', q.get('grants')!);
  if (q.get('ttl')) params.set('ttl', q.get('ttl')!);
  const res = await fetch(`/token?${params}`);
  const token = await res.text();
  state.dataset.state = 'connecting';
  await room.connect(q.get('ws')!, token, { autoSubscribe: q.get('autosub') !== '0' });
  record('connected', { version: room.serverInfo?.version, protocol: room.serverInfo?.protocol });
  if (q.get('publish') === '1') {
    await room.localParticipant.setMicrophoneEnabled(true);
    await room.localParticipant.setCameraEnabled(true);
    record('published');
  }
  state.dataset.state = room.state === ConnectionState.Connected ? 'connected' : room.state;
}

main().catch((e) => {
  record('error', String(e));
  state.dataset.state = 'error';
  state.textContent = String(e);
});
