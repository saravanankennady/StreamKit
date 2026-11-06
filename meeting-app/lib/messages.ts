// Chat and reactions travel as data messages between the browsers in a meeting. The server stamps each one with its
// real sender, so a message cannot claim to be from someone else, and what it carries is treated as untrusted text.
import { type Participant, type Room, RoomEvent } from "@elnova-streamkit/client";
import { useCallback, useEffect, useRef, useState } from "react";

const CHAT_TOPIC = "meeting.chat";
const REACTION_TOPIC = "meeting.reaction";
export const MAX_CHAT_LENGTH = 2000;
const REACTION_SECONDS = 4;
// Nothing else limits how many of these someone in the meeting can send to this page, which has to draw them all. Past
// this, the oldest go. Chat is not kept anyway.
const MAX_CHAT_MESSAGES = 500;
const MAX_REACTIONS_SHOWN = 30;

export const REACTIONS = [
  { emoji: "👍", label: "Thumbs up" },
  { emoji: "👏", label: "Clapping" },
  { emoji: "😂", label: "Laughing" },
  { emoji: "😮", label: "Surprised" },
  { emoji: "❤️", label: "Love" },
  { emoji: "🎉", label: "Celebrating" },
] as const;

const REACTION_SET = new Set<string>(REACTIONS.map((r) => r.emoji));

export interface ChatMessage {
  id: string;
  from: string;
  mine: boolean;
  text: string;
  at: number;
}

export interface Reaction {
  id: string;
  from: string;
  emoji: string;
}

let sequence = 0;
const nextId = () => `${Date.now()}-${++sequence}`;

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function keepLatest<T>(all: T[], limit: number): T[] {
  return all.length > limit ? all.slice(all.length - limit) : all;
}

function displayName(p: Participant | undefined, identity: string | undefined): string {
  return p?.name || p?.identity || identity || "Someone";
}

/** Reads a message that claims to be JSON, from whoever sent it. Anything unexpected reads as nothing. */
function parse(payload: Uint8Array): Record<string, unknown> | null {
  try {
    const v = JSON.parse(decoder.decode(payload));
    return v && typeof v === "object" ? v : null;
  } catch {
    return null;
  }
}

/** Chat for as long as this person is in the meeting. Nothing is stored, so someone joining later starts from now. */
export function useChat(room: Room, panelOpen: boolean) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [unread, setUnread] = useState(0);
  const open = useRef(panelOpen);
  open.current = panelOpen;

  useEffect(() => {
    const onData = (payload: Uint8Array, from: Participant | undefined, _kind: unknown, topic: string | undefined, _enc: unknown, identity: string | undefined) => {
      if (topic !== CHAT_TOPIC) return;
      const text = parse(payload)?.text;
      if (typeof text !== "string" || text.trim() === "") return;
      setMessages((all) => keepLatest([...all, { id: nextId(), from: displayName(from, identity), mine: false, text: text.slice(0, MAX_CHAT_LENGTH), at: Date.now() }], MAX_CHAT_MESSAGES));
      if (!open.current) setUnread((n) => n + 1);
    };
    room.on(RoomEvent.DataReceived, onData);
    return () => {
      room.off(RoomEvent.DataReceived, onData);
    };
  }, [room]);

  useEffect(() => {
    if (panelOpen) setUnread(0);
  }, [panelOpen]);

  const send = useCallback(
    async (text: string) => {
      await room.localParticipant.publishData(encoder.encode(JSON.stringify({ text })), { reliable: true, topic: CHAT_TOPIC });
      setMessages((all) => keepLatest([...all, { id: nextId(), from: "You", mine: true, text, at: Date.now() }], MAX_CHAT_MESSAGES));
    },
    [room],
  );

  return { messages, unread, send };
}

/** Reactions float up for a few seconds, for everyone, and leave nothing behind. */
export function useReactions(room: Room) {
  const [reactions, setReactions] = useState<Reaction[]>([]);

  const show = useCallback((from: string, emoji: string) => {
    const id = nextId();
    setReactions((all) => keepLatest([...all, { id, from, emoji }], MAX_REACTIONS_SHOWN));
    setTimeout(() => setReactions((all) => all.filter((r) => r.id !== id)), REACTION_SECONDS * 1000);
  }, []);

  useEffect(() => {
    const onData = (payload: Uint8Array, from: Participant | undefined, _kind: unknown, topic: string | undefined, _enc: unknown, identity: string | undefined) => {
      if (topic !== REACTION_TOPIC) return;
      const emoji = parse(payload)?.emoji;
      // Only what the picker offers, so a reaction cannot be used to put arbitrary text over someone's video.
      if (typeof emoji === "string" && REACTION_SET.has(emoji)) show(displayName(from, identity), emoji);
    };
    room.on(RoomEvent.DataReceived, onData);
    return () => {
      room.off(RoomEvent.DataReceived, onData);
    };
  }, [room, show]);

  const send = useCallback(
    async (emoji: string) => {
      await room.localParticipant.publishData(encoder.encode(JSON.stringify({ emoji })), { reliable: true, topic: REACTION_TOPIC });
      show("You", emoji);
    },
    [room, show],
  );

  return { reactions, send };
}
