import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import {
  ConnectionQuality,
  ConnectionState,
  ParticipantEvent,
  RemoteParticipant,
  Room,
  RoomEvent,
  TrackKind,
  TrackPublication,
  TrackSource,
} from '@elnova-streamkit/client';
import { act, cleanup, render, renderHook, screen, waitFor } from '@testing-library/react';
import { StrictMode, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  AudioTrack,
  ParticipantTile,
  RoomAudioRenderer,
  RoomContext,
  RoomProvider,
  VideoTrack,
  getTrackReferenceId,
  useActiveSpeakers,
  useConnectionQuality,
  useConnectionState,
  useDataChannel,
  useLocalParticipant,
  useMaybeRoomContext,
  useMediaDevices,
  useParticipants,
  useRoom,
  useRoomContext,
  useTracks,
} from '../src/index.js';

function remote(room: Room, identity: string) {
  const p = new RemoteParticipant(() => undefined, () => true, `PA_${identity}`, identity);
  room.remoteParticipants.set(identity, p);
  return p;
}

function publication(p: { addPublication(pub: TrackPublication): void }, sid: string, source: TrackSource, kind = TrackKind.Video, track?: unknown) {
  const pub = new TrackPublication(kind, sid, source);
  pub.source = source;
  if (track) pub.track = track as never;
  (p as any).addPublication(pub);
  return pub;
}

const fakeTrack = (kind: string, isLocal = false) => ({
  kind,
  isLocal,
  attach: vi.fn(),
  detach: vi.fn(),
});

const withRoom = (room: Room) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return <RoomContext.Provider value={room}>{children}</RoomContext.Provider>;
  };

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('context', () => {
  it('useRoomContext needs a provider, and useMaybeRoomContext does not', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => renderHook(() => useRoomContext())).toThrow(/RoomProvider/);
    expect(renderHook(() => useMaybeRoomContext()).result.current).toBeUndefined();
  });
  it('gives the room from the provider', () => {
    const room = new Room();
    expect(renderHook(() => useRoomContext(), { wrapper: withRoom(room) }).result.current).toBe(room);
  });
});

describe('useConnectionState', () => {
  it('follows the event', () => {
    const room = new Room();
    const { result } = renderHook(() => useConnectionState(), { wrapper: withRoom(room) });
    expect(result.current).toBe(ConnectionState.Disconnected);
    act(() => {
      (room as any).state = ConnectionState.Connected;
      room.emit(RoomEvent.ConnectionStateChanged, ConnectionState.Connected);
    });
    expect(result.current).toBe(ConnectionState.Connected);
  });
});

describe('useParticipants', () => {
  it('starts with this client, and updates on events', () => {
    const room = new Room();
    const { result } = renderHook(() => useParticipants(), { wrapper: withRoom(room) });
    expect(result.current).toEqual([room.localParticipant]);
    let alice: RemoteParticipant;
    act(() => {
      alice = remote(room, 'alice');
      room.emit(RoomEvent.ParticipantConnected, alice);
    });
    expect(result.current.map((p) => p.identity)).toEqual(['', 'alice']);
    act(() => {
      room.remoteParticipants.delete('alice');
      room.emit(RoomEvent.ParticipantDisconnected, alice);
    });
    expect(result.current).toEqual([room.localParticipant]);
  });

  it('keeps the same array when nothing changed', () => {
    const room = new Room();
    const { result, rerender } = renderHook(() => useParticipants(), { wrapper: withRoom(room) });
    const first = result.current;
    rerender();
    expect(result.current).toBe(first);
  });

  it('stops listening on unmount', () => {
    const room = new Room();
    const { unmount } = renderHook(() => useParticipants(), { wrapper: withRoom(room) });
    expect(room.listenerCount(RoomEvent.ParticipantConnected)).toBe(1);
    unmount();
    expect(room.listenerCount(RoomEvent.ParticipantConnected)).toBe(0);
  });
});

describe('useLocalParticipant', () => {
  it('reports what is on, and updates on events', () => {
    const room = new Room();
    const lp = room.localParticipant;
    const { result } = renderHook(() => useLocalParticipant(), { wrapper: withRoom(room) });
    expect(result.current.isCameraEnabled).toBe(false);
    act(() => {
      const pub = publication(lp, 'TR_1', TrackSource.Camera);
      lp.emit(ParticipantEvent.LocalTrackPublished, pub as never);
    });
    expect(result.current.isCameraEnabled).toBe(true);
    expect(result.current.cameraTrack?.trackSid).toBe('TR_1');
  });
});

describe('useActiveSpeakers and useConnectionQuality', () => {
  it('follow their events', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const speakers = renderHook(() => useActiveSpeakers(), { wrapper: withRoom(room) });
    const quality = renderHook(() => useConnectionQuality(alice), { wrapper: withRoom(room) });
    expect(speakers.result.current).toEqual([]);
    expect(quality.result.current).toBe(ConnectionQuality.Unknown);
    act(() => {
      room.activeSpeakers = [alice];
      room.emit(RoomEvent.ActiveSpeakersChanged, [alice]);
      alice.setConnectionQuality(ConnectionQuality.Good);
    });
    expect(speakers.result.current).toEqual([alice]);
    expect(quality.result.current).toBe(ConnectionQuality.Good);
  });
});

describe('useTracks', () => {
  it('by default returns only what is being received', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    publication(alice, 'TR_1', TrackSource.Camera);
    const { result } = renderHook(() => useTracks([TrackSource.Camera]), { wrapper: withRoom(room) });
    expect(result.current).toEqual([]);
    act(() => {
      alice.trackPublications.get('TR_1')!.track = fakeTrack('video') as never;
      room.emit(RoomEvent.TrackSubscribed, {} as never, {} as never, alice);
    });
    expect(result.current.map((r) => r.publication?.trackSid)).toEqual(['TR_1']);
  });

  it('can include what is published and not received', () => {
    const room = new Room();
    publication(remote(room, 'alice'), 'TR_1', TrackSource.Camera);
    const { result } = renderHook(() => useTracks([TrackSource.Camera], { onlySubscribed: false }), { wrapper: withRoom(room) });
    expect(result.current).toHaveLength(1);
  });

  it('holds a place for someone without the track, if asked', () => {
    const room = new Room();
    remote(room, 'alice');
    const { result } = renderHook(() => useTracks([{ source: TrackSource.Camera, withPlaceholder: true }]), { wrapper: withRoom(room) });
    expect(result.current).toHaveLength(2);
    expect(result.current.every((r) => r.publication === undefined)).toBe(true);
    expect(getTrackReferenceId(result.current[1])).toBe('alice_camera_placeholder');
  });

  it('covers every source by default, and gives stable IDs', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    publication(alice, 'TR_1', TrackSource.Camera, TrackKind.Video, fakeTrack('video'));
    publication(alice, 'TR_2', TrackSource.Microphone, TrackKind.Audio, fakeTrack('audio'));
    const { result } = renderHook(() => useTracks(), { wrapper: withRoom(room) });
    expect(result.current.map(getTrackReferenceId).sort()).toEqual(['alice_camera_TR_1', 'alice_microphone_TR_2']);
  });
});

describe('VideoTrack', () => {
  it('attaches once, detaches on unmount, and goes through attach rather than srcObject', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const track = fakeTrack('video');
    const pub = publication(alice, 'TR_1', TrackSource.Camera, TrackKind.Video, track);
    const { container, unmount, rerender } = render(<VideoTrack trackRef={{ participant: alice, publication: pub, source: TrackSource.Camera }} />);
    rerender(<VideoTrack trackRef={{ participant: alice, publication: pub, source: TrackSource.Camera }} />);
    const el = container.querySelector('video')!;
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(track.attach).toHaveBeenCalledWith(el);
    expect(el.muted).toBe(true);
    unmount();
    expect(track.detach).toHaveBeenCalledWith(el);
  });

  it('attaches when a subscription arrives, and detaches when it goes', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const pub = publication(alice, 'TR_1', TrackSource.Camera);
    render(<VideoTrack trackRef={{ participant: alice, publication: pub, source: TrackSource.Camera }} />);
    const track = fakeTrack('video');
    act(() => {
      pub.track = track as never;
      alice.emit(ParticipantEvent.TrackSubscribed, track as never, pub as never);
    });
    expect(track.attach).toHaveBeenCalledTimes(1);
    act(() => {
      pub.track = undefined;
      alice.emit(ParticipantEvent.TrackUnsubscribed, track as never, pub as never);
    });
    expect(track.detach).toHaveBeenCalledTimes(1);
  });
});

describe('AudioTrack', () => {
  it('never plays one\'s own audio', () => {
    const room = new Room();
    const lp = room.localParticipant;
    const track = fakeTrack('audio', true);
    const pub = publication(lp, 'TR_1', TrackSource.Microphone, TrackKind.Audio, track);
    render(<AudioTrack trackRef={{ participant: lp, publication: pub, source: TrackSource.Microphone }} />);
    expect(track.attach).not.toHaveBeenCalled();
  });
});

describe('RoomAudioRenderer', () => {
  it('plays remote, subscribed audio and nothing else', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const subscribed = fakeTrack('audio');
    publication(alice, 'TR_a', TrackSource.Microphone, TrackKind.Audio, subscribed);
    publication(remote(room, 'bob'), 'TR_b', TrackSource.Microphone, TrackKind.Audio);
    const own = fakeTrack('audio', true);
    publication(room.localParticipant, 'TR_me', TrackSource.Microphone, TrackKind.Audio, own);
    const { container } = render(<RoomAudioRenderer />, { wrapper: withRoom(room) });
    const audios = container.querySelectorAll('audio');
    expect(audios).toHaveLength(1);
    expect((audios[0] as HTMLElement).dataset.identity).toBe('alice');
    expect(subscribed.attach).toHaveBeenCalledTimes(1);
    expect(own.attach).not.toHaveBeenCalled();
  });

  it('adds and removes as subscriptions change', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const pub = publication(alice, 'TR_a', TrackSource.Microphone, TrackKind.Audio);
    const { container } = render(<RoomAudioRenderer />, { wrapper: withRoom(room) });
    expect(container.querySelectorAll('audio')).toHaveLength(0);
    act(() => {
      pub.track = fakeTrack('audio') as never;
      room.emit(RoomEvent.TrackSubscribed, pub.track as never, pub as never, alice);
    });
    expect(container.querySelectorAll('audio')).toHaveLength(1);
    act(() => {
      pub.track = undefined;
      room.emit(RoomEvent.TrackUnsubscribed, {} as never, pub as never, alice);
    });
    expect(container.querySelectorAll('audio')).toHaveLength(0);
  });
});

describe('ParticipantTile', () => {
  it('shows a placeholder and a name until there is video, and state on data attributes', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    const { container } = render(<ParticipantTile participant={alice} />, { wrapper: withRoom(room) });
    const tile = container.querySelector('[data-streamkit-participant-tile]') as HTMLElement;
    expect(tile.dataset.identity).toBe('alice');
    expect(tile.dataset.speaking).toBe('false');
    expect(tile.dataset.videoMuted).toBe('true');
    expect(container.querySelector('[data-streamkit-placeholder]')).not.toBeNull();
    expect(screen.getByText('alice')).toBeTruthy();

    const track = fakeTrack('video');
    act(() => {
      const pub = publication(alice, 'TR_1', TrackSource.Camera, TrackKind.Video, track);
      alice.emit(ParticipantEvent.TrackSubscribed, track as never, pub as never);
      alice.setIsSpeaking(true);
    });
    expect(container.querySelector('video')).not.toBeNull();
    expect(container.querySelector('[data-streamkit-placeholder]')).toBeNull();
    expect(tile.dataset.speaking).toBe('true');
    expect(track.attach).toHaveBeenCalledTimes(1);
  });

  it('takes custom content, with the participant in context', () => {
    const room = new Room();
    const alice = remote(room, 'alice');
    function Name() {
      return <span>{useConnectionQuality()}</span>;
    }
    render(
      <ParticipantTile participant={alice}>
        <Name />
      </ParticipantTile>,
      { wrapper: withRoom(room) },
    );
    expect(screen.getByText('unknown')).toBeTruthy();
  });
});

describe('useDataChannel', () => {
  it('receives by topic, reaching the callback once per message', () => {
    const room = new Room();
    const seen: string[] = [];
    const { result } = renderHook(() => useDataChannel('chat', (m) => seen.push(m.text())), { wrapper: withRoom(room) });
    const encode = (s: string) => new TextEncoder().encode(s);
    act(() => {
      room.emit(RoomEvent.DataReceived, encode('one'), undefined, 0, 'chat', undefined, undefined);
      room.emit(RoomEvent.DataReceived, encode('two'), undefined, 0, 'other', undefined, undefined);
      room.emit(RoomEvent.DataReceived, encode('three'), undefined, 0, 'chat', undefined, undefined);
    });
    expect(seen).toEqual(['one', 'three']);
    expect(result.current.message?.text()).toBe('three');
  });

  it('with no topic, receives everything', () => {
    const room = new Room();
    const { result } = renderHook(() => useDataChannel(), { wrapper: withRoom(room) });
    act(() => {
      room.emit(RoomEvent.DataReceived, new Uint8Array([1]), undefined, 0, 'anything', undefined, undefined);
    });
    expect(result.current.message?.topic).toBe('anything');
  });

  it('sends text or bytes on its topic, and says while it is', async () => {
    const room = new Room();
    let finish!: () => void;
    const publish = vi.spyOn(room.localParticipant, 'publishData').mockImplementation(() => new Promise<void>((r) => (finish = r)));
    const { result } = renderHook(() => useDataChannel('chat'), { wrapper: withRoom(room) });
    let done!: Promise<void>;
    act(() => {
      done = result.current.send('hi', { reliable: false });
    });
    expect(result.current.isSending).toBe(true);
    expect(publish).toHaveBeenCalledWith(new TextEncoder().encode('hi'), { topic: 'chat', reliable: false });
    await act(async () => {
      finish();
      await done;
    });
    expect(result.current.isSending).toBe(false);
  });
});

describe('useMediaDevices', () => {
  let listeners: Array<() => void>;
  let devices: MediaDeviceInfo[];

  beforeEach(() => {
    listeners = [];
    devices = [{ deviceId: 'a', kind: 'videoinput', label: 'A' } as MediaDeviceInfo];
    vi.stubGlobal('navigator', {
      ...navigator,
      mediaDevices: {
        enumerateDevices: vi.fn(async () => devices),
        getUserMedia: vi.fn(),
        addEventListener: (_: string, fn: () => void) => listeners.push(fn),
        removeEventListener: (_: string, fn: () => void) => {
          listeners = listeners.filter((l) => l !== fn);
        },
      },
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it('lists devices, and updates when they change, without polling', async () => {
    const { result, unmount } = renderHook(() => useMediaDevices({ kind: 'videoinput' }));
    await waitFor(() => expect(result.current.map((d) => d.deviceId)).toEqual(['a']));
    devices = [...devices, { deviceId: 'b', kind: 'videoinput', label: 'B' } as MediaDeviceInfo];
    await act(async () => {
      listeners.forEach((l) => l());
    });
    await waitFor(() => expect(result.current).toHaveLength(2));
    unmount();
    expect(listeners).toEqual([]);
  });
});

describe('RoomProvider and useRoom', () => {
  function managedRoom() {
    const room = new Room();
    const connect = vi.spyOn(room, 'connect').mockImplementation(async () => {
      (room as any).state = ConnectionState.Connected;
    });
    const disconnect = vi.spyOn(room, 'disconnect').mockImplementation(async () => {
      (room as any).state = ConnectionState.Disconnected;
    });
    return { room, connect, disconnect };
  }

  it('connects, publishes when signalling is up, and leaves on unmount', async () => {
    const { room, connect, disconnect } = managedRoom();
    const mic = vi.spyOn(room.localParticipant, 'setMicrophoneEnabled').mockResolvedValue(undefined);
    const cam = vi.spyOn(room.localParticipant, 'setCameraEnabled').mockResolvedValue(undefined);
    const { unmount } = render(
      <RoomProvider room={room} serverUrl="ws://x" token="t" audio video>
        <div />
      </RoomProvider>,
    );
    await waitFor(() => expect(connect).toHaveBeenCalledWith('ws://x', 't', undefined));
    expect(mic).not.toHaveBeenCalled();
    act(() => {
      room.emit(RoomEvent.SignalConnected);
    });
    expect(mic).toHaveBeenCalledWith(true, undefined);
    expect(cam).toHaveBeenCalledWith(true, undefined);
    unmount();
    expect(disconnect).toHaveBeenCalled();
  });

  it('does nothing until it has a URL and a token', () => {
    const { room, connect } = managedRoom();
    render(<RoomProvider room={room} serverUrl="ws://x" />);
    expect(connect).not.toHaveBeenCalled();
  });

  it('follows the connect prop', async () => {
    const { room, connect, disconnect } = managedRoom();
    const { rerender } = render(<RoomProvider room={room} serverUrl="ws://x" token="t" connect={false} />);
    expect(connect).not.toHaveBeenCalled();
    rerender(<RoomProvider room={room} serverUrl="ws://x" token="t" connect />);
    await waitFor(() => expect(connect).toHaveBeenCalledTimes(1));
    disconnect.mockClear();
    rerender(<RoomProvider room={room} serverUrl="ws://x" token="t" connect={false} />);
    expect(disconnect).toHaveBeenCalledTimes(1);
  });

  it('source props changed while connected take effect', () => {
    const { room } = managedRoom();
    const cam = vi.spyOn(room.localParticipant, 'setCameraEnabled').mockResolvedValue(undefined);
    const { rerender } = render(<RoomProvider room={room} serverUrl="ws://x" token="t" video />);
    act(() => {
      room.emit(RoomEvent.SignalConnected);
    });
    expect(cam).toHaveBeenLastCalledWith(true, undefined);
    rerender(<RoomProvider room={room} serverUrl="ws://x" token="t" video={false} />);
    expect(cam).toHaveBeenLastCalledWith(false, undefined);
  });

  it('reports connection errors, but not the attempt it cancelled itself', async () => {
    const { room, connect } = managedRoom();
    connect.mockRejectedValueOnce(new Error('nope'));
    const onError = vi.fn();
    const { result } = renderHook(() => useRoom({ room, serverUrl: 'ws://x', token: 't', onError }));
    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(result.current.error?.message).toBe('nope');

    const { room: other, connect: otherConnect } = managedRoom();
    otherConnect.mockRejectedValueOnce(Object.assign(new Error('cancelled'), { reason: 'cancelled' }));
    const quiet = vi.fn();
    renderHook(() => useRoom({ room: other, serverUrl: 'ws://x', token: 't', onError: quiet }));
    await new Promise((r) => setTimeout(r));
    expect(quiet).not.toHaveBeenCalled();
  });

  it('in StrictMode ends up connected, however it got there', async () => {
    const { room, connect, disconnect } = managedRoom();
    render(
      <StrictMode>
        <RoomProvider room={room} serverUrl="ws://x" token="t" />
      </StrictMode>,
    );
    await waitFor(() => expect(connect).toHaveBeenCalled());
    // Connect, leave, connect: the last thing that happened has to be a connect.
    const lastConnect = Math.max(...connect.mock.invocationCallOrder);
    const lastDisconnect = Math.max(0, ...disconnect.mock.invocationCallOrder);
    expect(lastConnect).toBeGreaterThan(lastDisconnect);
  });

  it('calls back when connected and disconnected', () => {
    const { room } = managedRoom();
    const onConnected = vi.fn();
    const onDisconnected = vi.fn();
    render(<RoomProvider room={room} serverUrl="ws://x" token="t" onConnected={onConnected} onDisconnected={onDisconnected} />);
    act(() => {
      room.emit(RoomEvent.Connected);
      room.emit(RoomEvent.Disconnected, 4);
    });
    expect(onConnected).toHaveBeenCalledTimes(1);
    expect(onDisconnected).toHaveBeenCalledWith(4);
  });

  it('makes a room of its own from options, and a new one if they change', () => {
    const seen: Room[] = [];
    function Capture() {
      seen.push(useRoomContext());
      return null;
    }
    const { rerender } = render(
      <RoomProvider options={{ dynacast: false }}>
        <Capture />
      </RoomProvider>,
    );
    rerender(
      <RoomProvider options={{ dynacast: false }}>
        <Capture />
      </RoomProvider>,
    );
    expect(new Set(seen).size).toBe(1);
    rerender(
      <RoomProvider options={{ dynacast: true }}>
        <Capture />
      </RoomProvider>,
    );
    expect(new Set(seen).size).toBe(2);
  });
});

describe('state comes from events', () => {
  it('nothing in the package polls', () => {
    const walk = (dir: string): string[] =>
      readdirSync(dir).flatMap((f) => {
        const path = join(dir, f);
        return statSync(path).isDirectory() ? walk(path) : [path];
      });
    for (const file of walk(join(__dirname, '../src'))) {
      const source = readFileSync(file, 'utf8');
      expect(source, file).not.toMatch(/setInterval|requestAnimationFrame/);
    }
  });
});
