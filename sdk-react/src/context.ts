import type { Participant, Room } from '@elnova-streamkit/client';
import { createContext, useContext } from 'react';
import type { TrackReferenceOrPlaceholder } from './trackReference.js';

export const RoomContext = createContext<Room | undefined>(undefined);
export const ParticipantContext = createContext<Participant | undefined>(undefined);
export const TrackRefContext = createContext<TrackReferenceOrPlaceholder | undefined>(undefined);

/** The room from the nearest provider. It is an error to call this outside one. */
export function useRoomContext(): Room {
  const room = useContext(RoomContext);
  if (!room) throw new Error('useRoomContext must be used inside a RoomProvider');
  return room;
}

export function useMaybeRoomContext(): Room | undefined {
  return useContext(RoomContext);
}

/** The room given, or the provider's. */
export function useEnsureRoom(room?: Room): Room {
  const context = useContext(RoomContext);
  const out = room ?? context;
  if (!out) throw new Error('no room: pass one, or use this inside a RoomProvider');
  return out;
}

export function useMaybeParticipantContext(): Participant | undefined {
  return useContext(ParticipantContext);
}

/** The participant given, or the one from context. */
export function useEnsureParticipant(participant?: Participant): Participant {
  const context = useContext(ParticipantContext);
  const trackRef = useContext(TrackRefContext);
  const out = participant ?? context ?? trackRef?.participant;
  if (!out) throw new Error('no participant: pass one, or use this inside a ParticipantTile');
  return out;
}

export function useMaybeTrackRefContext(): TrackReferenceOrPlaceholder | undefined {
  return useContext(TrackRefContext);
}

export function useEnsureTrackRef(trackRef?: TrackReferenceOrPlaceholder): TrackReferenceOrPlaceholder {
  const context = useContext(TrackRefContext);
  const out = trackRef ?? context;
  if (!out) throw new Error('no track reference: pass one, or use this inside a TrackRefContext');
  return out;
}
