// How a guest gets into a meeting: a share link's code is swapped for a short-lived access token. The token stays in
// memory, and the API secret that signs it never leaves the server.

export type MeetingRole = "host" | "moderator" | "guest";

export interface LinkInfo {
  room: string;
  label: string;
  can_publish: boolean;
  expires_at: string;
  role: MeetingRole;
  /** The meeting's title, if a host has given it one. It goes before the link's own label. */
  title: string;
  locked: boolean;
  waiting_room: boolean;
  ended: boolean;
}

export interface JoinSession {
  token: string;
  url: string;
  room: string;
  identity: string;
  role: MeetingRole;
}

/** A place in the waiting room. Having one gets nobody in: a host decides, and a token comes after that. */
export interface WaitingPlace {
  status: "waiting";
  ticket: string;
  room: string;
}

export class JoinError extends Error {
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.name = "JoinError";
    this.code = code;
  }
}

async function post<T>(path: string, body: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method: "POST",
      // A guest is nobody as far as the API goes, even if this browser is also signed in to the dashboard.
      credentials: "omit",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new JoinError("unreachable", "Could not reach the server. Check your connection and try again.");
  }
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    throw new JoinError(data?.error?.code ?? "unknown", data?.error?.message ?? "Something went wrong on the server. Try again in a moment.");
  }
  return data as T;
}

export const previewLink = (code: string) => post<LinkInfo>("/join/preview", { code });
export const joinWithLink = (code: string, name: string) => post<JoinSession | WaitingPlace>("/join", { code, name });

export function isWaiting(answer: JoinSession | WaitingPlace): answer is WaitingPlace {
  return "ticket" in answer;
}

/** Asks whether a place in the waiting room has turned into a way in. */
export const checkTicket = (ticket: string) => post<{ status: "waiting" } | (JoinSession & { status: "admitted" })>("/join/wait", { ticket });

/** Gives up a place in the waiting room. Failing is harmless: it lapses by itself once nothing asks about it. */
export function leaveWaitingRoom(ticket: string) {
  void fetch("/api/v1/join/wait", {
    method: "POST",
    credentials: "omit",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ticket, cancel: true }),
    keepalive: true,
  }).catch(() => {});
}

export function meetingTitle(link: Pick<LinkInfo, "title" | "label" | "room">): string {
  return link.title || link.label || link.room;
}

/** The server's address can be set to localhost, which only works on the computer running it. */
export function serverOnlyReachableLocally(serverUrl: string): boolean {
  try {
    const host = new URL(serverUrl).hostname;
    const local = (h: string) => h === "localhost" || h === "127.0.0.1" || h === "[::1]";
    return local(host) && !local(location.hostname);
  } catch {
    return false;
  }
}
