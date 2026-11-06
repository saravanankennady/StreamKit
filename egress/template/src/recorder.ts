// The page streamkit-egress records. It joins a room as a hidden participant using our SDK, lays video out on a canvas, mixes
// audio with WebAudio, and records both with MediaRecorder. Everything comes from one browser clock, which is what keeps
// audio and video in sync. Chunks go to the worker over a local WebSocket.
import {
  DisconnectReason,
  ParticipantKind,
  RemoteParticipant,
  RemoteTrack,
  RemoteTrackPublication,
  Room,
  RoomEvent,
  TrackKind,
  TrackSource,
  setLogLevel,
  LogLevel,
} from '@elnova-streamkit/client';
import { fitSource, gridLayout, speakerLayout, type Placed } from './layout.js';

interface Config {
  url: string;
  token: string;
  type: 'room_composite' | 'participant' | 'audio_only' | 'video_only';
  participant?: string;
  layout: 'grid' | 'speaker';
  width: number;
  height: number;
  fps: number;
  videoBitrate: number;
  audioBitrate: number;
  keyframeMs: number;
  timesliceMs: number;
  mimeTypes: string[];
  videoGraceMs: number;
}

interface TileState {
  key: string;
  identity: string;
  name: string;
  video?: HTMLVideoElement;
  isScreen: boolean;
}

const hash = new URLSearchParams(location.hash.slice(1));
const bridge = new WebSocket(`ws://${location.host}/bridge?token=${encodeURIComponent(hash.get('token') ?? '')}`);
bridge.binaryType = 'arraybuffer';
history.replaceState(null, '', location.pathname);

function say(message: Record<string, unknown>) {
  if (bridge.readyState === WebSocket.OPEN) bridge.send(JSON.stringify(message));
}
const log = (level: string, message: string) => say({ type: 'log', level, message });

window.addEventListener('error', (e) => log('error', `page error: ${e.message}`));
window.addEventListener('unhandledrejection', (e) => log('error', `unhandled rejection: ${String(e.reason)}`));

let cfg: Config;
let room: Room;
let recorder: MediaRecorder | undefined;
let bytes = 0;
let framesDrawn = 0;
let finishing = false;
let endReason = 'stop';
let endDetail = '';
let firstTrackAt = 0;

const canvas = document.getElementById('stage') as HTMLCanvasElement;
const mediaBox = document.getElementById('media') as HTMLDivElement;
let g: CanvasRenderingContext2D;

const videos = new Map<string, { el: HTMLVideoElement; publication: RemoteTrackPublication; participant: RemoteParticipant }>();
const audios = new Map<string, { el: HTMLMediaElement; node: MediaStreamAudioSourceNode; track: RemoteTrack }>();
let audioCtx: AudioContext | undefined;
let mixBus: AudioNode | undefined;
let mixDest: MediaStreamAudioDestinationNode | undefined;
let mixMeter: AnalyserNode | undefined;
let audioPeak = 0;
let lastSpeaker: string | undefined;

const wantsVideo = () => cfg.type !== 'audio_only';
const wantsAudio = () => cfg.type !== 'video_only';

function wanted(participant: RemoteParticipant, kind: TrackKind): boolean {
  if (participant.kind === ParticipantKind.EGRESS) return false;
  if (cfg.participant && participant.identity !== cfg.participant) return false;
  return kind === TrackKind.Audio ? wantsAudio() : wantsVideo();
}

function setupAudio() {
  audioCtx = new AudioContext({ sampleRate: 48000, latencyHint: 'playback' });
  mixDest = audioCtx.createMediaStreamDestination();
  // Several loud people at once would otherwise clip.
  const limiter = audioCtx.createDynamicsCompressor();
  limiter.threshold.value = -3;
  limiter.knee.value = 0;
  limiter.ratio.value = 20;
  limiter.attack.value = 0.003;
  limiter.release.value = 0.25;
  limiter.connect(mixDest);
  mixBus = limiter;
  mixMeter = audioCtx.createAnalyser();
  mixMeter.fftSize = 2048;
  limiter.connect(mixMeter);
  // A silent source keeps the output running while nobody is talking, so the timeline has no holes.
  const silence = audioCtx.createConstantSource();
  silence.offset.value = 0;
  silence.connect(limiter);
  silence.start();
  void audioCtx.resume();
}

function onSubscribed(track: RemoteTrack, publication: RemoteTrackPublication, participant: RemoteParticipant) {
  if (!wanted(participant, track.kind)) return;
  firstTrackAt ||= performance.now();
  if (track.kind === TrackKind.Video) {
    const el = track.attach() as HTMLVideoElement;
    el.muted = true;
    el.playsInline = true;
    mediaBox.appendChild(el);
    void el.play().catch(() => {});
    videos.set(publication.trackSid, { el, publication, participant });
  } else if (audioCtx && mixBus) {
    // Chrome only delivers a remote track to WebAudio while a media element is playing it. Muted is fine.
    const el = track.attach();
    el.muted = true;
    mediaBox.appendChild(el);
    void el.play().catch(() => {});
    const node = audioCtx.createMediaStreamSource(new MediaStream([track.mediaStreamTrack]));
    node.connect(mixBus);
    audios.set(publication.trackSid, { el, node, track });
  }
  say({ type: 'log', level: 'info', message: `${participant.identity} ${publication.source}: recording` });
  maybeStart();
}

// How loud the mix has been since the last report. Silence is a normal thing for a room to do, but a worker log that can tell
// it apart from audio not arriving is what makes a silent recording possible to explain.
function measureMix() {
  if (!mixMeter) return;
  const samples = new Float32Array(mixMeter.fftSize);
  mixMeter.getFloatTimeDomainData(samples);
  for (const s of samples) audioPeak = Math.max(audioPeak, Math.abs(s));
}

function audioElements() {
  let playing = 0;
  let muted = 0;
  for (const { el, track } of audios.values()) {
    if (!el.paused && el.readyState >= 2) playing++;
    if (track.mediaStreamTrack.muted) muted++;
  }
  return { playing, muted };
}

// What is arriving for each audio track. Along with the mix's level, this says where sound stopped, if it does: packets that
// are not arriving, samples that nothing is pulling out of them, or samples that do not reach the mix.
async function audioArriving() {
  const out: { packets: number; samples: number; energy: number }[] = [];
  for (const { track } of audios.values()) {
    const report: RTCStatsReport | undefined = await (track as any).getRTCStatsReport?.();
    report?.forEach((s: any) => {
      if (s.type === 'inbound-rtp') out.push({ packets: s.packetsReceived ?? 0, samples: s.totalSamplesReceived ?? 0, energy: Math.round((s.totalAudioEnergy ?? 0) * 1000) / 1000 });
    });
  }
  return out;
}

function onUnsubscribed(track: RemoteTrack, publication: RemoteTrackPublication) {
  const video = videos.get(publication.trackSid);
  if (video) {
    track.detach(video.el);
    video.el.remove();
    videos.delete(publication.trackSid);
  }
  const audio = audios.get(publication.trackSid);
  if (audio) {
    audio.node.disconnect();
    track.detach(audio.el);
    audio.el.remove();
    audios.delete(publication.trackSid);
  }
}

// With filtering on, the recorder asks for exactly what it needs and nothing else is ever sent to it.
function subscribeIfWanted(publication: RemoteTrackPublication, participant: RemoteParticipant) {
  if (wanted(participant, publication.kind)) publication.setSubscribed(true);
}

function tiles(): TileState[] {
  const out: TileState[] = [];
  const seen = new Set<string>();
  for (const v of videos.values()) {
    const isScreen = v.publication.source === TrackSource.ScreenShare;
    const name = v.participant.name || v.participant.identity;
    out.push({ key: v.publication.trackSid, identity: v.participant.identity, name: isScreen ? `${name} (screen)` : name, video: v.el, isScreen });
    if (!isScreen) seen.add(v.participant.identity);
  }
  // People with no camera on still appear, or a grid would not show who is in the room.
  for (const p of room.remoteParticipants.values()) {
    if (p.kind === ParticipantKind.EGRESS || seen.has(p.identity)) continue;
    if (cfg.participant && p.identity !== cfg.participant) continue;
    out.push({ key: `p:${p.identity}`, identity: p.identity, name: p.name || p.identity, isScreen: false });
  }
  out.sort((a, b) => a.identity.localeCompare(b.identity) || Number(a.isScreen) - Number(b.isScreen));
  return out;
}

function arrange(list: TileState[]): Placed<TileState>[] {
  const { width: W, height: H } = canvas;
  const screen = list.find((t) => t.isScreen);
  if (screen) return speakerLayout(screen, list.filter((t) => t !== screen), W, H);
  if (cfg.layout === 'speaker') {
    const main = list.find((t) => t.identity === lastSpeaker) ?? list[0];
    return speakerLayout(main, list.filter((t) => t !== main), W, H);
  }
  return gridLayout(list, W, H);
}

function draw() {
  const { width: W, height: H } = canvas;
  g.fillStyle = '#101114';
  g.fillRect(0, 0, W, H);
  for (const { tile, x, y, w, h } of arrange(tiles())) {
    const rect = { x, y, w, h };
    const v = tile.video;
    if (v && v.videoWidth > 0) {
      const s = fitSource(v.videoWidth, v.videoHeight, rect, !tile.isScreen);
      g.drawImage(v, s.sx, s.sy, s.sw, s.sh, s.dx, s.dy, s.dw, s.dh);
    } else {
      g.fillStyle = '#23252b';
      g.fillRect(x, y, w, h);
      g.fillStyle = '#c8ccd4';
      g.font = `600 ${Math.round(Math.min(w, h) / 4)}px sans-serif`;
      g.textAlign = 'center';
      g.textBaseline = 'middle';
      g.fillText(tile.name.slice(0, 1).toUpperCase(), x + w / 2, y + h / 2);
    }
    if (w > 160) {
      const size = Math.max(12, Math.round(h / 22));
      g.font = `500 ${size}px sans-serif`;
      g.textAlign = 'left';
      g.textBaseline = 'alphabetic';
      const label = tile.name.length > 40 ? `${tile.name.slice(0, 39)}…` : tile.name;
      const metrics = g.measureText(label);
      g.fillStyle = 'rgba(0,0,0,0.55)';
      g.fillRect(x + 8, y + h - size - 16, metrics.width + 16, size + 10);
      g.fillStyle = '#fff';
      g.fillText(label, x + 16, y + h - 13);
    }
  }
  // Canvas capture only produces a frame when something was drawn, so a still scene needs a change no one can see.
  g.fillStyle = framesDrawn % 2 ? '#101114' : '#101115';
  g.fillRect(0, 0, 1, 1);
  framesDrawn++;
}

/** Recording starts once there is something to record: an empty room produces no file. */
function readyToStart(): boolean {
  if (wantsVideo()) {
    for (const v of videos.values()) if (v.el.videoWidth > 0) return true;
    // Someone who is only audio should not wait forever behind video that is not coming. Video-only has no choice.
    if (wantsAudio() && audios.size > 0 && performance.now() - firstTrackAt > cfg.videoGraceMs) return true;
    return false;
  }
  return audios.size > 0;
}

function maybeStart() {
  if (recorder || finishing || !readyToStart()) return;
  const tracks: MediaStreamTrack[] = [];
  if (wantsVideo()) {
    const stream = canvas.captureStream(cfg.fps);
    tracks.push(...stream.getVideoTracks());
  }
  if (wantsAudio() && mixDest) tracks.push(...mixDest.stream.getAudioTracks());
  const mimeType = cfg.mimeTypes.find((m) => MediaRecorder.isTypeSupported(m));
  if (!mimeType) {
    say({ type: 'error', message: `this browser cannot record any of: ${cfg.mimeTypes.join(', ')}` });
    return;
  }
  const options: MediaRecorderOptions & { videoKeyFrameIntervalDuration?: number } = { mimeType, audioBitsPerSecond: cfg.audioBitrate };
  if (wantsVideo()) {
    options.videoBitsPerSecond = cfg.videoBitrate;
    options.videoKeyFrameIntervalDuration = cfg.keyframeMs;
  }
  recorder = new MediaRecorder(new MediaStream(tracks), options);
  recorder.ondataavailable = (e) => {
    if (e.data.size > 0) {
      bytes += e.data.size;
      bridge.send(e.data);
    }
  };
  recorder.onerror = (e) => say({ type: 'error', message: `MediaRecorder failed: ${(e as any).error?.message ?? 'unknown error'}` });
  recorder.onstop = () => say({ type: 'finished', reason: endReason, message: endDetail, recorded: true });
  recorder.start(cfg.timesliceMs);
  say({ type: 'recording', mime: mimeType });
}

function finish(reason: string, detail = '') {
  if (finishing) return;
  finishing = true;
  endReason = reason;
  endDetail = detail;
  if (recorder && recorder.state !== 'inactive') {
    recorder.stop();
  } else {
    say({ type: 'finished', reason, message: detail, recorded: false });
  }
}

// Only the room ending is an ending. A server restarting, the recorder being removed, or a connection that could not be
// brought back all leave a meeting going on without being recorded, and the worker has to report that as a failure.
function onDisconnected(reason?: DisconnectReason) {
  if (reason === DisconnectReason.ROOM_CLOSED || reason === DisconnectReason.ROOM_DELETED) {
    finish('room_closed');
  } else {
    finish('disconnected', reason === undefined ? 'CONNECTION_LOST' : DisconnectReason[reason]);
  }
}

async function run(config: Config) {
  cfg = config;
  setLogLevel(LogLevel.warn);
  canvas.width = cfg.width;
  canvas.height = cfg.height;
  // Reading pixels back is what software rendering does anyway. This keeps the canvas from going through a GPU
  // process that has no GPU behind it.
  g = canvas.getContext('2d', { alpha: false, willReadFrequently: true })!;
  room = new Room({ adaptiveStream: false, dynacast: false });
  draw();
  if (wantsAudio()) setupAudio();

  room.on(RoomEvent.TrackSubscribed, onSubscribed);
  room.on(RoomEvent.TrackUnsubscribed, onUnsubscribed);
  room.on(RoomEvent.TrackPublished, subscribeIfWanted);
  room.on(RoomEvent.TrackSubscriptionFailed, (sid) => log('warn', `could not subscribe to ${sid}`));
  room.on(RoomEvent.ParticipantConnected, (p) => log('info', `${p.identity} joined`));
  room.on(RoomEvent.ParticipantDisconnected, (p) => log('info', `${p.identity} left`));
  room.on(RoomEvent.ActiveSpeakersChanged, (speakers) => {
    const first = speakers.find((s) => s.identity !== room.localParticipant.identity);
    if (first) lastSpeaker = first.identity;
  });
  room.on(RoomEvent.Disconnected, onDisconnected);

  await room.connect(cfg.url, cfg.token, { autoSubscribe: false });
  for (const p of room.remoteParticipants.values()) {
    for (const publication of p.getTrackPublications()) subscribeIfWanted(publication, p);
  }
  say({ type: 'connected' });

  setInterval(() => {
    if (wantsVideo()) draw();
    measureMix();
    maybeStart();
  }, 1000 / cfg.fps);
  setInterval(async () => {
    const { playing, muted } = audioElements();
    const peak = Math.round(audioPeak * 1000) / 1000;
    audioPeak = 0;
    say({
      type: 'stats', framesDrawn, bytes, videoTracks: videos.size, audioTracks: audios.size, audioState: audioCtx?.state, audioTime: audioCtx?.currentTime,
      audioPeak: peak, audioPlaying: playing, audioMuted: muted, audioIn: await audioArriving().catch(() => []),
    });
  }, 2000);
}

bridge.onmessage = (e) => {
  if (typeof e.data !== 'string') return;
  const message = JSON.parse(e.data);
  if (message.type === 'config') {
    run(message.config).catch((err) => say({ type: 'error', message: `could not join the room: ${String(err?.message ?? err)}` }));
  } else if (message.type === 'stop') {
    finish('stop');
  }
};
bridge.onclose = () => {
  // The worker has gone. Nothing can receive the recording any more, so there is no reason to stay in the room.
  void room?.disconnect();
};
