import {
  type AudioCaptureOptions,
  ConnectionState,
  type DisconnectReason,
  Room,
  type RoomConnectOptions,
  RoomEvent,
  type RoomOptions,
  type ScreenShareCaptureOptions,
  type VideoCaptureOptions,
} from '@elnova-streamkit/client';
import { useEffect, useMemo, useRef, useState } from 'react';

export interface UseRoomOptions {
  serverUrl?: string;
  token?: string;
  /** Join the room. Setting this to false leaves it. Defaults to true. */
  connect?: boolean;
  /** Publish the microphone. Changing this while connected turns it on and off. */
  audio?: boolean | AudioCaptureOptions;
  /** Publish the camera. Changing this while connected turns it on and off. */
  video?: boolean | VideoCaptureOptions;
  /** Share the screen. Changing this while connected starts and stops it. */
  screen?: boolean | ScreenShareCaptureOptions;
  options?: RoomOptions;
  connectOptions?: RoomConnectOptions;
  /** A room to manage, in place of a new one. */
  room?: Room;
  onConnected?: () => void;
  onDisconnected?: (reason?: DisconnectReason) => void;
  onError?: (error: Error) => void;
}

export interface UseRoomResult {
  room: Room;
  /** The latest error from connecting or publishing, cleared by the next attempt. */
  error?: Error;
}

/**
 * Owns a room's connection for the lifetime of a component: joins when it mounts with what it needs, leaves
 * when it unmounts. React's development double-mount is safe, because a room can reconnect straight after
 * leaving, and whatever the first mount started is cancelled.
 */
export function useRoom(props: UseRoomOptions): UseRoomResult {
  const { serverUrl, token, connect = true, audio = false, video = false, screen = false, options, connectOptions, room: given } = props;
  const optionsKey = JSON.stringify(options ?? null);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const room = useMemo(() => given ?? new Room(options), [given, optionsKey]);
  const [error, setError] = useState<Error>();

  const callbacks = useRef(props);
  callbacks.current = props;

  const report = (e: unknown) => {
    const err = e instanceof Error ? e : new Error(String(e));
    setError(err);
    callbacks.current.onError?.(err);
  };
  const reportRef = useRef(report);
  reportRef.current = report;

  useEffect(() => {
    const onConnected = () => callbacks.current.onConnected?.();
    const onDisconnected = (reason?: DisconnectReason) => callbacks.current.onDisconnected?.(reason);
    room.on(RoomEvent.Connected, onConnected);
    room.on(RoomEvent.Disconnected, onDisconnected);
    return () => {
      room.off(RoomEvent.Connected, onConnected);
      room.off(RoomEvent.Disconnected, onDisconnected);
    };
  }, [room]);

  // Publishing starts when signalling is up, without waiting for the connections: a server accepts tracks
  // from that point, and this puts the camera on as early as can be. Afterwards it follows the props.
  const sourcesKey = JSON.stringify([audio, video, screen]);
  const publishable = useRef(false);
  useEffect(() => {
    const apply = () => {
      const lp = room.localParticipant;
      const fail = (e: unknown) => {
        // Leaving cancels what was in flight, which is nothing to report.
        if (room.state !== ConnectionState.Disconnected) reportRef.current(e);
      };
      lp.setMicrophoneEnabled(!!audio, typeof audio === 'object' ? audio : undefined).catch(fail);
      lp.setCameraEnabled(!!video, typeof video === 'object' ? video : undefined).catch(fail);
      lp.setScreenShareEnabled(!!screen, typeof screen === 'object' ? screen : undefined).catch(fail);
    };
    const onSignal = () => {
      publishable.current = true;
      apply();
    };
    const onGone = () => {
      publishable.current = false;
    };
    room.on(RoomEvent.SignalConnected, onSignal);
    room.on(RoomEvent.Disconnected, onGone);
    if (publishable.current) apply();
    return () => {
      room.off(RoomEvent.SignalConnected, onSignal);
      room.off(RoomEvent.Disconnected, onGone);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, sourcesKey]);

  const connectKey = JSON.stringify(connectOptions ?? null);
  useEffect(() => {
    if (!connect) {
      if (room.state !== ConnectionState.Disconnected) void room.disconnect();
      return;
    }
    if (!serverUrl || !token) return;
    setError(undefined);
    room.connect(serverUrl, token, connectOptions).catch((e) => {
      // Leaving while connecting cancels the attempt, and that is not a failure anyone needs to see.
      if (e?.reason !== 'cancelled') reportRef.current(e);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, connect, serverUrl, token, connectKey]);

  useEffect(() => {
    return () => {
      void room.disconnect();
    };
  }, [room]);

  return { room, error };
}
