// What the meeting page says to the server once someone is in a meeting. Every call carries the caller's own access
// token and nothing else: what it may do is decided by the server, from what the server has on record for them. The
// controls a person sees are only a convenience, and hiding one secures nothing.
import type { Participant, Room } from "@elnova-streamkit/client";
import { useEffect, useReducer } from "react";
import { JoinError, type MeetingRole } from "./join";

export const ROLE_ATTRIBUTE = "meeting.role";
export const HAND_ATTRIBUTE = "meeting.hand";

/** What everyone in a meeting is told about it, through the room's metadata. */
export interface MeetingState {
  title: string;
  locked: boolean;
  waiting_room: boolean;
}

export function parseMeetingState(raw: string | undefined): MeetingState {
  const empty = { title: "", locked: false, waiting_room: false };
  if (!raw) return empty;
  try {
    const v = JSON.parse(raw);
    return { title: typeof v?.title === "string" ? v.title : "", locked: v?.locked === true, waiting_room: v?.waiting_room === true };
  } catch {
    // Metadata set through the API can be anything, and what it says about a meeting is only for showing.
    return empty;
  }
}

export interface WaitingPerson {
  identity: string;
  name: string;
}

/** What those running a meeting can also read. */
export interface MeetingDetails {
  role: MeetingRole;
  /** The ones who have waited longest. waiting_count says how many there are in all. */
  waiting?: WaitingPerson[];
  waiting_count?: number;
  invite_url?: string;
  recording?: { id: string; status: string };
}

export interface MeetingSummary {
  title: string;
  started_at: string;
  ended_at: string | null;
  peak_participants: number;
  people: { name: string; role: string; joined_at: string; left_at: string | null }[];
  recordings: { status: string; duration_ms: number; file_size: number }[];
}

async function send<T>(token: string | undefined, method: string, path: string, body?: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      // The token says who is calling. A dashboard session in this browser must not change that.
      credentials: "omit",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${token ?? ""}` },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new JoinError("unreachable", "Could not reach the server. Check your connection and try again.");
  }
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new JoinError(data?.error?.code ?? "unknown", data?.error?.message ?? "Something went wrong on the server. Try again in a moment.");
  return data as T;
}

export function meetingApi(room: Room, roomName: string) {
  const base = `/meetings/${encodeURIComponent(roomName)}`;
  const call = <T,>(method: string, path: string, body?: unknown) => send<T>(room.accessToken, method, path, body);
  const person = (identity: string) => `${base}/participants/${encodeURIComponent(identity)}`;
  return {
    details: () => call<MeetingDetails>("GET", base),
    update: (change: Partial<MeetingState>) => call<void>("PATCH", base, change),
    end: () => call<void>("POST", `${base}/end`, {}),
    muteAll: () => call<{ muted: number }>("POST", `${base}/mute-all`, {}),
    setHand: (raised: boolean) => call<void>("POST", `${base}/hand`, { raised }),
    answer: (identity: string, admit: boolean) => call<void>("POST", `${base}/waiting/${encodeURIComponent(identity)}`, { admit }),
    turnAwayEveryone: () => call<{ turned_away: number }>("DELETE", `${base}/waiting`),
    mute: (identity: string) => call<void>("POST", `${person(identity)}/mute`, {}),
    remove: (identity: string) => call<void>("POST", `${person(identity)}/remove`, {}),
    lowerHand: (identity: string) => call<void>("POST", `${person(identity)}/lower-hand`, {}),
    setRole: (identity: string, role: "moderator" | "guest") => call<void>("PATCH", person(identity), { role }),
    startRecording: () => call<{ id: string }>("POST", "/egress", { type: "room_composite", room: roomName, layout: "grid", output: { kind: "file", format: "mp4" } }),
    stopRecording: (id: string) => call<void>("POST", `/egress/${encodeURIComponent(id)}/stop`, {}),
  };
}

export type MeetingApi = ReturnType<typeof meetingApi>;

/** The factual record of a meeting, for the end screen. It works after leaving, for as long as the token does. */
export const fetchSummary = (token: string, roomName: string) => send<MeetingSummary>(token, "GET", `/meetings/${encodeURIComponent(roomName)}/summary`);

export function roleOf(p: Participant): MeetingRole {
  const role = p.attributes[ROLE_ATTRIBUTE];
  return role === "host" || role === "moderator" ? role : "guest";
}

export function handRaised(p: Participant): boolean {
  return Boolean(p.attributes[HAND_ATTRIBUTE]);
}

export function canModerate(role: MeetingRole): boolean {
  return role === "host" || role === "moderator";
}

/** Re-renders when any of these events fire. Whatever is read from the SDK at render time then stays current. */
export function useEvents(emitter: { on(e: never, l: () => void): unknown; off(e: never, l: () => void): unknown }, events: readonly string[]) {
  const [, bump] = useReducer((n: number) => n + 1, 0);
  const key = events.join(" ");
  useEffect(() => {
    const names = key.split(" ") as never[];
    for (const e of names) emitter.on(e, bump);
    return () => {
      for (const e of names) emitter.off(e, bump);
    };
  }, [emitter, key]);
}

export function initials(name: string): string {
  const words = name.trim().split(/\s+/).slice(0, 2);
  return words.map((w) => Array.from(w)[0] ?? "").join("").toUpperCase() || "?";
}
