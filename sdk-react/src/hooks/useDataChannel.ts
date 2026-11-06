import { type Participant, type Room, RoomEvent } from '@elnova-streamkit/client';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useEnsureRoom } from '../context.js';

export interface DataMessage {
  payload: Uint8Array;
  topic?: string;
  from?: Participant;
  /** The message as text, decoded on request since many payloads are not text. */
  text(): string;
}

export interface DataSendOptions {
  reliable?: boolean;
  destinationIdentities?: string[];
  topic?: string;
}

export interface UseDataChannelResult {
  /** The latest message received. */
  message?: DataMessage;
  send: (payload: Uint8Array | string, options?: DataSendOptions) => Promise<void>;
  isSending: boolean;
}

/**
 * Sends and receives room data. With a topic, only messages on that topic arrive and everything sent goes on
 * that topic. Each message reaches the callback even when several land in one render.
 */
export function useDataChannel(topic?: string, onMessage?: (message: DataMessage) => void, options: { room?: Room } = {}): UseDataChannelResult {
  const room = useEnsureRoom(options.room);
  const [message, setMessage] = useState<DataMessage>();
  const [sending, setSending] = useState(0);
  const callback = useRef(onMessage);
  callback.current = onMessage;

  useEffect(() => {
    const handler = (payload: Uint8Array, from: Participant | undefined, _kind: unknown, msgTopic: string | undefined) => {
      if (topic !== undefined && msgTopic !== topic) return;
      const m: DataMessage = { payload, topic: msgTopic, from, text: () => new TextDecoder().decode(payload) };
      setMessage(m);
      callback.current?.(m);
    };
    room.on(RoomEvent.DataReceived, handler);
    return () => {
      room.off(RoomEvent.DataReceived, handler);
    };
  }, [room, topic]);

  const send = useCallback(
    async (payload: Uint8Array | string, opts: DataSendOptions = {}) => {
      const bytes = typeof payload === 'string' ? new TextEncoder().encode(payload) : payload;
      setSending((n) => n + 1);
      try {
        await room.localParticipant.publishData(bytes, { topic, ...opts });
      } finally {
        setSending((n) => n - 1);
      }
    },
    [room, topic],
  );

  return { message, send, isSending: sending > 0 };
}
