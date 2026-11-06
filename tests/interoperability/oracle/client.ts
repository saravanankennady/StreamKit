// Black-box test client: the official livekit-client SDK, driven by URL parameters.
// Used only by the interoperability suite. Elnova StreamKit's own SDK lives in sdk-js/.
import { ConnectionQuality, ConnectionState, DataPacket_Kind, Room, RoomEvent, Track, VideoQuality } from 'livekit-client';

const q = new URLSearchParams(location.search);
const state = document.getElementById('state')!;
const events: Array<{ t: number; name: string; detail?: unknown }> = [];
const started = performance.now();

function record(name: string, detail?: unknown) {
  events.push({ t: Math.round(performance.now() - started), name, detail });
}

const publishDefaults: Record<string, unknown> = {};
if (q.get('videoCodec')) publishDefaults.videoCodec = q.get('videoCodec');
if (q.get('scalabilityMode')) publishDefaults.scalabilityMode = q.get('scalabilityMode');
if (q.get('backupCodec')) publishDefaults.backupCodec = q.get('backupCodec') === '1';

const room = new Room({
  singlePeerConnection: q.get('mode') !== 'dual',
  adaptiveStream: q.get('adaptive') === '1',
  dynacast: q.get('dynacast') === '1',
  publishDefaults,
});

for (const name of Object.values(RoomEvent)) {
  room.on(name as RoomEvent, (...args: unknown[]) => {
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

room.on(RoomEvent.TrackSubscribed, (track) => {
  const el = track.attach();
  el.muted = true;
  el.dataset.source = track.source;
  el.dataset.kind = track.kind;
  el.dataset.sid = track.sid ?? '';
  document.getElementById('media')!.appendChild(el);
  void (el as HTMLMediaElement).play().catch(() => {});
});

room.on(RoomEvent.TrackUnsubscribed, (_track, publication) => {
  // The SDK has already detached the track by now, so find its elements by ID.
  document.querySelectorAll(`[data-sid="${publication.trackSid}"]`).forEach((el) => el.remove());
});

async function subscribedStats() {
  const out: unknown[] = [];
  for (const p of room.remoteParticipants.values()) {
    for (const pub of p.trackPublications.values()) {
      const t = pub.track;
      if (!t) continue;
      const report = await t.getRTCStatsReport();
      const inbound: any[] = [];
      report?.forEach((s: any) => {
        if (s.type === 'inbound-rtp') inbound.push(s);
      });
      const r = inbound[0] ?? {};
      const codec = r.codecId ? report?.get(r.codecId) : undefined;
      out.push({
        mimeType: codec?.mimeType ?? null,
        framesDropped: r.framesDropped ?? 0,
        freezeCount: r.freezeCount ?? 0,
        keyFramesDecoded: r.keyFramesDecoded ?? 0,
        pliCount: r.pliCount ?? 0,
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

// What this client's camera really sends: the browser's view, not what the signalling claims.
async function publishedVideo() {
  const pub = [...room.localParticipant.trackPublications.values()].find((p) => p.kind === Track.Kind.Video);
  const sender = pub?.track?.sender;
  if (!sender) return null;
  const params = sender.getParameters();
  const report = await sender.getStats();
  const outbound: any[] = [];
  report.forEach((s: any) => {
    if (s.type === 'outbound-rtp' && s.kind === 'video') outbound.push(s);
  });
  const active = outbound.find((s) => s.codecId);
  const codec = active ? report.get(active.codecId) : undefined;
  return {
    sid: pub!.trackSid,
    mimeType: codec?.mimeType ?? null,
    scalabilityModes: params.encodings.map((e: any) => e.scalabilityMode ?? null),
    rids: params.encodings.map((e: any) => e.rid ?? null),
    bytesSent: outbound.reduce((n, s) => n + (s.bytesSent ?? 0), 0),
    framesEncoded: outbound.reduce((n, s) => n + (s.framesEncoded ?? 0), 0),
    streams: outbound.length,
    limitedBy: active?.qualityLimitationReason ?? null,
    frameHeight: active?.frameHeight ?? null,
  };
}

// Hash of a video element's current frame. Two different hashes prove pixels really moved.
function frameHash(sid?: string) {
  const v = document.querySelector<HTMLVideoElement>(
    sid ? `video[data-sid="${sid}"]` : 'video[data-kind="video"]',
  );
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

(window as any).harness = {
  room,
  events,
  subscribedStats,
  publishedVideo,
  frameHash,
  received,
  disconnectReason: () => disconnectReason,
  Track,
  VideoQuality,
  DataPacket_Kind,
  ConnectionQuality,
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
};

async function main() {
  // A test supplies its own token and connects by itself.
  if (q.get('noconnect') === '1') {
    state.dataset.state = 'idle';
    return;
  }
  const identity = q.get('identity') ?? 'anon';
  const roomName = q.get('room') ?? 'oracle';
  const res = await fetch(`/token?identity=${encodeURIComponent(identity)}&room=${encodeURIComponent(roomName)}`);
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
