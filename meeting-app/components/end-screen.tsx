import { DisconnectReason } from "@elnova-streamkit/client";
import { useEffect, useState } from "react";
import { type LinkInfo, type MeetingRole, meetingTitle } from "@/lib/join";
import { fetchSummary, type MeetingSummary } from "@/lib/meeting";
import { Button, Centered } from "./ui";

export interface MeetingEnd {
  /** Why it ended, when it had started. */
  reason?: DisconnectReason;
  /** Why it never started, or stopped working. */
  failure?: string;
  seconds: number;
  /** How many other people were there at some point. */
  others: number;
  /** What those people called themselves. */
  names: string[];
  /** Whether this person saw a recording running. */
  recorded: boolean;
  token?: string;
  room: string;
  role: MeetingRole;
}

const HEADLINES: Partial<Record<DisconnectReason, string>> = {
  [DisconnectReason.CLIENT_INITIATED]: "You left the meeting",
  [DisconnectReason.PARTICIPANT_REMOVED]: "You were removed from the meeting",
  [DisconnectReason.ROOM_DELETED]: "The meeting was closed",
  [DisconnectReason.ROOM_CLOSED]: "The meeting was closed",
  [DisconnectReason.SERVER_SHUTDOWN]: "The server shut down",
};

// The server's history is written just after things happen, so someone leaving can arrive ahead of their own departure.
const SECOND_LOOK_MS = 2000;

function headline(end: MeetingEnd): string {
  if (end.failure) return "Could not join the meeting";
  return (end.reason !== undefined && HEADLINES[end.reason]) || "You were disconnected";
}

function duration(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  if (minutes === 0) return `${seconds} ${seconds === 1 ? "second" : "seconds"}`;
  return `${minutes} ${minutes === 1 ? "minute" : "minutes"}`;
}

/** What the server has on record. Without it, the screen still says what this browser saw for itself. */
function useSummary(end: MeetingEnd): MeetingSummary | null {
  const [summary, setSummary] = useState<MeetingSummary | null>(null);
  const { token, room } = end;
  useEffect(() => {
    if (!token || end.failure || end.reason === DisconnectReason.PARTICIPANT_REMOVED) return;
    let live = true;
    const look = () => {
      fetchSummary(token, room)
        .then((s) => {
          if (live) setSummary(s);
        })
        .catch(() => {});
    };
    look();
    const timer = setTimeout(look, SECOND_LOOK_MS);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [token, room, end.failure, end.reason]);
  return summary;
}

/** Facts from what the server and this browser saw. Nothing here is interpreted: there is no transcript to work from. */
function Summary({ end, summary }: { end: MeetingEnd; summary: MeetingSummary | null }) {
  const meetingSeconds = summary ? Math.max(0, Math.round(((summary.ended_at ? new Date(summary.ended_at).getTime() : Date.now()) - new Date(summary.started_at).getTime()) / 1000)) : null;
  const people = summary ? [...new Set(summary.people.map((p) => p.name).filter(Boolean))] : end.names;
  const recordings = summary?.recordings ?? [];
  const recorded = end.recorded || recordings.length > 0;
  return (
    <dl data-testid="end-summary" className="mt-4 grid grid-cols-2 gap-3 text-sm">
      <div>
        <dt className="text-ink-3">You were here for</dt>
        <dd data-testid="end-duration">{duration(end.seconds)}</dd>
      </div>
      <div>
        <dt className="text-ink-3">Other people</dt>
        <dd data-testid="end-others">{end.others}</dd>
      </div>
      {meetingSeconds !== null && (
        <div>
          <dt className="text-ink-3">{summary?.ended_at ? "The meeting ran for" : "So far the meeting has run for"}</dt>
          <dd data-testid="end-meeting-duration">{duration(meetingSeconds)}</dd>
        </div>
      )}
      <div>
        <dt className="text-ink-3">Recorded</dt>
        <dd data-testid="end-recorded" data-recorded={recorded}>
          {recorded ? `Yes${recordings.length > 1 ? `, ${recordings.length} recordings` : ""}` : "No"}
        </dd>
      </div>
      {people.length > 0 && (
        <div className="col-span-2">
          <dt className="text-ink-3">Who was there</dt>
          <dd data-testid="end-people" className="break-words">
            {people.join(", ")}
          </dd>
        </div>
      )}
      {recorded && (end.role === "host" || end.role === "moderator") && (
        <p className="col-span-2 text-xs text-ink-3" data-testid="end-recording-note">
          People in this project can find the recording on the Recordings page of the dashboard.
        </p>
      )}
    </dl>
  );
}

export function EndScreen({ link, end, onRejoin }: { link: LinkInfo; end: MeetingEnd; onRejoin: () => void }) {
  const summary = useSummary(end);
  const closed = end.reason === DisconnectReason.ROOM_DELETED || end.reason === DisconnectReason.ROOM_CLOSED;
  // Someone removed could use the link again, because it does not name them. Their token is what the server refuses,
  // and a button inviting them back would only be rude. A meeting that was closed has nothing to go back to, unless
  // this person can start it again.
  const canRejoin = end.reason !== DisconnectReason.PARTICIPANT_REMOVED && (!closed || end.role === "host");
  return (
    <Centered>
      <div data-testid="end-screen">
        <h1 className="text-xl font-semibold">{headline(end)}</h1>
        <p className="mt-1 text-sm text-ink-2">{summary?.title || meetingTitle(link)}</p>
        {end.failure ? (
          <p role="alert" data-testid="join-failure" className="mt-3 text-sm text-critical-text">
            {end.failure}
          </p>
        ) : (
          <Summary end={end} summary={summary} />
        )}
        {canRejoin && (
          <Button variant="primary" className="mt-5" data-testid="rejoin" onClick={onRejoin}>
            {end.failure ? "Try again" : closed ? "Start the meeting again" : "Rejoin"}
          </Button>
        )}
      </div>
    </Centered>
  );
}
