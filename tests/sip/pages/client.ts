// Someone in a room, using the official client library in real Chrome. It publishes a tone it makes itself, and measures what it
// really decodes from each track. So a pass means audio got across and came out of a real decoder with the right content.
import { LocalAudioTrack, Room, RoomEvent, Track } from 'livekit-client';

declare global {
  interface Window {
    MediaStreamTrackProcessor: any;
    harness: any;
  }
}

const RATE = 48000;
const room = new Room({ adaptiveStream: false, dynacast: false });

// The last few seconds of what each track really decoded to.
const captured = new Map<string, Float32Array[]>();
const digits: { digit: string; code: number; from: string }[] = [];
const left: string[] = [];
const disconnects: string[] = [];

room.on(RoomEvent.TrackSubscribed, (track, _pub, participant) => {
  if (track.kind !== Track.Kind.Audio) return;
  const el = track.attach();
  el.muted = true;
  document.body.appendChild(el);
  void el.play().catch(() => {});
  const processor = new window.MediaStreamTrackProcessor({ track: track.mediaStreamTrack });
  const reader = processor.readable.getReader();
  const chunks: Float32Array[] = [];
  captured.set(participant.identity, chunks);
  void (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      const data = new Float32Array(value.numberOfFrames);
      value.copyTo(data, { planeIndex: 0, format: 'f32-planar' });
      value.close();
      chunks.push(data);
      let total = chunks.reduce((n, c) => n + c.length, 0);
      while (total > RATE * 8 && chunks.length > 1) {
        total -= chunks.shift()!.length;
      }
    }
  })();
});
room.on(RoomEvent.SipDTMFReceived, (dtmf, participant) => {
  digits.push({ code: dtmf.code, digit: dtmf.digit, from: participant?.identity ?? '' });
});
room.on(RoomEvent.ParticipantDisconnected, (p) => left.push(p.identity));
room.on(RoomEvent.Disconnected, (reason) => disconnects.push(String(reason)));

function goertzel(pcm: Float32Array, freq: number) {
  let c = 0;
  let s = 0;
  for (let i = 0; i < pcm.length; i++) {
    const a = (2 * Math.PI * freq * i) / RATE;
    c += pcm[i] * Math.cos(a);
    s += pcm[i] * Math.sin(a);
  }
  return (2 * Math.hypot(c, s)) / Math.max(1, pcm.length);
}

function recent(identity: string, seconds: number) {
  const chunks = captured.get(identity) ?? [];
  const want = Math.floor(seconds * RATE);
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Float32Array(Math.min(want, total));
  let at = out.length;
  for (let i = chunks.length - 1; i >= 0 && at > 0; i--) {
    const c = chunks[i];
    const take = Math.min(c.length, at);
    out.set(c.subarray(c.length - take), at - take);
    at -= take;
  }
  return out;
}

let audio: AudioContext | undefined;
let oscillator: OscillatorNode | undefined;

window.harness = {
  async join(ws: string, token: string) {
    await room.connect(ws, token, { autoSubscribe: true });
    return { sid: room.localParticipant.sid, identity: room.localParticipant.identity, room: room.name };
  },
  // A tone made here, so its content is known exactly.
  async publishTone(freq: number) {
    audio = new AudioContext({ sampleRate: RATE });
    await audio.resume();
    oscillator = audio.createOscillator();
    oscillator.frequency.value = freq;
    const gain = audio.createGain();
    gain.gain.value = 0.3;
    const dest = audio.createMediaStreamDestination();
    oscillator.connect(gain).connect(dest);
    oscillator.start();
    const track = new LocalAudioTrack(dest.stream.getAudioTracks()[0], undefined, true);
    await room.localParticipant.publishTrack(track, { source: Track.Source.Microphone, name: 'tone' });
  },
  participants() {
    return [...room.remoteParticipants.values()].map((p) => ({ identity: p.identity, sid: p.sid, name: p.name, kind: p.kind, metadata: p.metadata, attributes: { ...p.attributes } }));
  },
  // The level of each frequency in the last seconds of a track, and the loudest overall.
  levels(identity: string, freqs: number[], seconds: number) {
    const pcm = recent(identity, seconds);
    let peak = 0;
    for (const v of pcm) peak = Math.max(peak, Math.abs(v));
    return { samples: pcm.length, peak, levels: Object.fromEntries(freqs.map((f) => [f, goertzel(pcm, f)])) };
  },
  digits: () => digits,
  left: () => left,
  disconnects: () => disconnects,
  sendDigit: (code: number, digit: string) => room.localParticipant.publishDtmf(code, digit),
  rpc: (destinationIdentity: string, method: string, payload: string) => room.localParticipant.performRpc({ destinationIdentity, method, payload }),
  leave: () => room.disconnect(),
};
document.body.dataset.ready = '1';
