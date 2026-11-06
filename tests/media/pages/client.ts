// A person in a call: our own SDK, with Chrome's fake camera and microphone. The tests drive it through window.harness.
import { ConnectionState, Room, RoomEvent, Track, TrackSource } from '@elnova-streamkit/client';

const q = new URLSearchParams(location.search);
const state = document.getElementById('state')!;
const media = document.getElementById('media')!;

const room = new Room({ adaptiveStream: false, dynacast: false });

room.on(RoomEvent.TrackSubscribed, (track, publication) => {
  const el = track.attach();
  el.dataset.sid = publication.trackSid;
  el.dataset.kind = track.kind;
  media.appendChild(el);
  void (el as HTMLMediaElement).play().catch(() => {});
});
room.on(RoomEvent.TrackUnsubscribed, (_track, publication) => {
  document.querySelectorAll(`[data-sid="${publication.trackSid}"]`).forEach((el) => el.remove());
});

// Everything arriving: decoded video frames and received audio samples prove media really flows.
async function received() {
  const out: any[] = [];
  for (const p of room.remoteParticipants.values()) {
    for (const pub of p.getTrackPublications()) {
      const t = pub.track as any;
      if (!t) continue;
      const report: RTCStatsReport | undefined = await t.getRTCStatsReport();
      let inbound: any = {};
      report?.forEach((s: any) => {
        if (s.type === 'inbound-rtp') inbound = s;
      });
      out.push({
        identity: p.identity,
        participantKind: p.kind,
        kind: t.kind,
        source: pub.source,
        sid: pub.trackSid,
        packetsReceived: inbound.packetsReceived ?? 0,
        bytesReceived: inbound.bytesReceived ?? 0,
        framesDecoded: inbound.framesDecoded ?? 0,
        totalSamplesReceived: inbound.totalSamplesReceived ?? 0,
        totalAudioEnergy: inbound.totalAudioEnergy ?? 0,
        width: inbound.frameWidth ?? 0,
        height: inbound.frameHeight ?? 0,
        fps: inbound.framesPerSecond ?? 0,
      });
    }
  }
  return out;
}

// Hash of a video's current frame. Two different hashes prove the pixels really moved.
function frameHash(sid: string) {
  const v = document.querySelector<HTMLVideoElement>(`video[data-sid="${sid}"]`);
  if (!v || !v.videoWidth) return null;
  const c = document.createElement('canvas');
  c.width = 64;
  c.height = 36;
  const g = c.getContext('2d', { willReadFrequently: true })!;
  g.drawImage(v, 0, 0, c.width, c.height);
  const d = g.getImageData(0, 0, c.width, c.height).data;
  let h = 2166136261;
  let min = 255;
  let max = 0;
  for (let i = 0; i < d.length; i += 4) {
    h = Math.imul(h ^ d[i] ^ d[i + 1] ^ d[i + 2], 16777619);
    const luma = (d[i] + d[i + 1] + d[i + 2]) / 3;
    if (luma < min) min = luma;
    if (luma > max) max = luma;
  }
  return { hash: h >>> 0, width: v.videoWidth, height: v.videoHeight, min, max };
}

(window as any).harness = {
  room,
  received,
  frameHash,
  Track,
  TrackSource,
  identities: () => [...room.remoteParticipants.keys()],
  publish: async () => {
    await room.localParticipant.setMicrophoneEnabled(true);
    await room.localParticipant.setCameraEnabled(true);
  },
  publishVideoOnly: () => room.localParticipant.setCameraEnabled(true),
  // A test pattern for measuring sync: once a second, the picture flashes white and a tone sounds, from the same timer.
  publishSyncPattern: async () => {
    const canvas = document.createElement('canvas');
    canvas.width = 640;
    canvas.height = 360;
    const g = canvas.getContext('2d')!;
    const ctx = new AudioContext({ sampleRate: 48000 });
    await ctx.resume();
    (window as any).harness.syncContext = ctx;
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    const dest = ctx.createMediaStreamDestination();
    gain.gain.value = 0;
    osc.frequency.value = 1000;
    osc.connect(gain).connect(dest);
    osc.start();
    g.fillStyle = '#000';
    g.fillRect(0, 0, 640, 360);
    const started = performance.now();
    setInterval(() => {
      const on = (performance.now() - started) % 1000 < 200;
      g.fillStyle = on ? '#fff' : '#000';
      g.fillRect(0, 0, 640, 360);
      gain.gain.value = on ? 0.8 : 0;
    }, 5);
    await room.localParticipant.publishTrack(canvas.captureStream(30).getVideoTracks()[0], { source: TrackSource.Camera, simulcast: false } as any);
    await room.localParticipant.publishTrack(dest.stream.getAudioTracks()[0], { source: TrackSource.Microphone } as any);
  },
  leave: () => room.disconnect(),
};

async function main() {
  await room.connect(q.get('ws')!, q.get('token')!, { autoSubscribe: q.get('autosub') !== '0' });
  if (q.get('publish') === '1') await (window as any).harness.publish();
  state.dataset.state = room.state === ConnectionState.Connected ? 'connected' : room.state;
}
main().catch((e) => {
  state.dataset.state = 'error';
  state.textContent = String(e);
});
