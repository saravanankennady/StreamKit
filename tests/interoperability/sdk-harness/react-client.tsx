// Test client for the React package: a small meeting page, built only from the package's public hooks and
// components, in StrictMode like an application in development. The test reads the page's DOM, so what it
// checks is what a user of the page would see.
import {
  ConnectionState,
  Room,
  RoomEvent,
  TrackSource,
  VideoQuality,
} from '@elnova-streamkit/client';
import {
  ParticipantTile,
  RoomAudioRenderer,
  RoomProvider,
  getTrackReferenceId,
  useActiveSpeakers,
  useConnectionQuality,
  useConnectionState,
  useDataChannel,
  useLocalParticipant,
  useMediaDeviceSelect,
  useMediaDevices,
  useParticipants,
  useRoomContext,
  useTracks,
} from '@elnova-streamkit/react';
import { StrictMode, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';

const q = new URLSearchParams(location.search);
const received: string[] = [];
let renders = 0;

function Status() {
  renders++;
  const room = useRoomContext();
  const state = useConnectionState();
  const participants = useParticipants();
  const { localParticipant, isCameraEnabled, isMicrophoneEnabled, isScreenShareEnabled } = useLocalParticipant();
  const speakers = useActiveSpeakers();
  const quality = useConnectionQuality(localParticipant);
  const { send, message } = useDataChannel('chat', (m) => received.push(m.text()));
  const devices = useMediaDevices({ kind: 'videoinput', requestPermissions: true });
  const select = useMediaDeviceSelect({ kind: 'audiooutput' });
  useEffect(() => {
    Object.assign((window as any).harness, { room, send, localParticipant });
  });
  return (
    <div id="status">
      <span id="connection">{state}</span>
      <span id="quality">{quality}</span>
      <span id="local" data-camera={isCameraEnabled} data-microphone={isMicrophoneEnabled} data-screen={isScreenShareEnabled} />
      <ul id="participants">
        {participants.map((p) => (
          <li key={p.identity} data-identity={p.identity} data-sid={p.sid}>
            {p.identity}
          </li>
        ))}
      </ul>
      <span id="speakers">{speakers.map((p) => p.identity).join(',')}</span>
      <span id="last-message">{message ? message.text() : ''}</span>
      <span id="devices">{devices.length}</span>
      <span id="audio-devices">{select.devices.length}</span>
    </div>
  );
}

function Grid() {
  const tracks = useTracks([{ source: TrackSource.Camera, withPlaceholder: true }, TrackSource.ScreenShare]);
  return (
    <div id="grid" data-count={tracks.length}>
      {tracks.map((ref) => (
        <ParticipantTile key={getTrackReferenceId(ref)} trackRef={ref} />
      ))}
    </div>
  );
}

function App() {
  const [token, setToken] = useState<string>();
  const [props, setProps] = useState({
    connect: true,
    audio: q.get('publish') === '1',
    video: q.get('publish') === '1',
    screen: false,
    mounted: true,
  });
  useEffect(() => {
    const params = new URLSearchParams({ identity: q.get('identity') ?? 'anon', room: q.get('room') ?? 'react' });
    void fetch(`/token?${params}`)
      .then((r) => r.text())
      .then(setToken);
    (window as any).harness.setProps = (next: Partial<typeof props>) => setProps((p) => ({ ...p, ...next }));
  }, []);
  if (!token || !props.mounted) return <div id="unmounted" />;
  return (
    <RoomProvider
      serverUrl={q.get('ws')!}
      token={token}
      connect={props.connect}
      audio={props.audio}
      video={props.video}
      screen={props.screen}
      options={{ adaptiveStream: q.get('adaptive') === '1' }}
      onConnected={() => {
        (window as any).harness.connectedCount++;
        document.getElementById('state')!.dataset.state = 'connected';
      }}
      onDisconnected={(reason) => {
        (window as any).harness.disconnectedWith = reason;
      }}
      onError={(e) => {
        document.getElementById('state')!.dataset.state = 'error';
        document.getElementById('state')!.textContent = String(e);
      }}
    >
      <Status />
      <Grid />
      <RoomAudioRenderer />
    </RoomProvider>
  );
}

function frameHash(identity: string, source = 'camera') {
  const v = document.querySelector<HTMLVideoElement>(`[data-streamkit-participant-tile] video[data-identity="${identity}"][data-source="${source}"]`);
  if (!v || !v.videoWidth) return null;
  const c = document.createElement('canvas');
  c.width = 64;
  c.height = 36;
  const g = c.getContext('2d', { willReadFrequently: true })!;
  g.drawImage(v, 0, 0, c.width, c.height);
  const d = g.getImageData(0, 0, c.width, c.height).data;
  let h = 2166136261;
  for (let i = 0; i < d.length; i += 4) h = Math.imul(h ^ d[i] ^ d[i + 1] ^ d[i + 2], 16777619);
  return { hash: h >>> 0, width: v.videoWidth, height: v.videoHeight, paused: v.paused };
}

(window as any).harness = {
  sdk: 'react',
  connectedCount: 0,
  received,
  renders: () => renders,
  frameHash,
  Room,
  RoomEvent,
  ConnectionState,
  VideoQuality,
  audioElements: () =>
    [...document.querySelectorAll<HTMLAudioElement>('[data-streamkit-room-audio] audio')].map((a) => ({
      identity: a.dataset.identity,
      live: (a.srcObject as MediaStream | null)?.getAudioTracks()[0]?.readyState === 'live',
      paused: a.paused,
    })),
  audioStats: async (identity: string) => {
    const room: Room = (window as any).harness.room;
    const pub = room.remoteParticipants.get(identity)?.getTrackPublication(TrackSource.Microphone);
    const report = await (pub?.track as any)?.getRTCStatsReport();
    let packets = 0;
    report?.forEach((s: any) => {
      if (s.type === 'inbound-rtp') packets = s.packetsReceived;
    });
    return packets;
  },
};

const root = document.getElementById('root')!;
createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
