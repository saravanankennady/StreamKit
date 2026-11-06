import type { ReactNode } from 'react';
import { RoomContext } from '../context.js';
import { useRoom, type UseRoomOptions } from '../hooks/useRoom.js';

export interface RoomProviderProps extends UseRoomOptions {
  children?: ReactNode;
}

/** Joins a room and makes it available to everything inside. */
export function RoomProvider({ children, ...props }: RoomProviderProps) {
  const { room } = useRoom(props);
  return <RoomContext.Provider value={room}>{children}</RoomContext.Provider>;
}
