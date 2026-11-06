import { Room, RoomEvent } from '@elnova-streamkit/client';
import { useCallback, useEffect, useState } from 'react';
import { useEventVersion } from '../useEvents.js';
import { useMaybeRoomContext } from '../context.js';

export interface UseMediaDevicesOptions {
  kind?: MediaDeviceKind;
  /** Ask for permission, so devices have names. Leave off to avoid a prompt, and take what is there. */
  requestPermissions?: boolean;
  onError?: (error: Error) => void;
}

/** The devices of a kind, kept current as they are plugged in and out. */
export function useMediaDevices({ kind, requestPermissions = false, onError }: UseMediaDevicesOptions = {}): MediaDeviceInfo[] {
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  useEffect(() => {
    let live = true;
    const load = () => {
      Room.getLocalDevices(kind, requestPermissions)
        .then((d) => {
          if (live) setDevices(d);
        })
        .catch((e) => onError?.(e));
    };
    load();
    navigator.mediaDevices?.addEventListener?.('devicechange', load);
    return () => {
      live = false;
      navigator.mediaDevices?.removeEventListener?.('devicechange', load);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, requestPermissions]);
  return devices;
}

const ACTIVE_EVENTS = [RoomEvent.ActiveDeviceChanged];

/** The devices of a kind, which one the room is using, and a way to switch. */
export function useMediaDeviceSelect(options: UseMediaDevicesOptions & { kind: MediaDeviceKind; room?: Room }) {
  const context = useMaybeRoomContext();
  const room = options.room ?? context;
  const devices = useMediaDevices(options);
  useEventVersion(room, ACTIVE_EVENTS);
  const setActiveDevice = useCallback(
    (deviceId: string, opts?: { exact?: boolean }) => {
      if (!room) throw new Error('no room to switch devices for');
      return room.switchActiveDevice(options.kind, deviceId, opts?.exact);
    },
    [room, options.kind],
  );
  return { devices, activeDeviceId: room?.getActiveDevice(options.kind), setActiveDevice };
}
