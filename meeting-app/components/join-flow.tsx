"use client";

import { useEffect, useState } from "react";
import { JoinError, type JoinSession, type LinkInfo, meetingTitle, previewLink } from "@/lib/join";
import { EndScreen, type MeetingEnd } from "./end-screen";
import { MeetingRoom } from "./meeting-room";
import { type JoinChoices, PreJoin } from "./pre-join";
import { Centered } from "./ui";
import { WaitingRoom } from "./waiting-room";

type Stage =
  | { name: "checking" }
  | { name: "unusable"; title: string; message: string }
  | { name: "prejoin"; link: LinkInfo }
  | { name: "waiting"; link: LinkInfo; ticket: string; choices: JoinChoices }
  | { name: "room"; link: LinkInfo; session: JoinSession; choices: JoinChoices }
  | { name: "ended"; link: LinkInfo; end: MeetingEnd };

const TITLES: Record<string, string> = {
  link_expired: "This link has expired",
  link_revoked: "This link has been turned off",
  link_not_found: "This link is not valid",
  unreachable: "Could not reach the server",
  meeting_ended: "This meeting has ended",
  meeting_locked: "This meeting is locked",
  waiting_denied: "The host did not let you in",
  ticket_not_found: "You are no longer waiting",
};

function readCode(): string {
  return location.hash.slice(1);
}

/**
 * A host link is a key. Once the page knows that is what it has, the code leaves the address bar, where it could be copied
 * with the address or seen while the screen is shared, and the browser's history. It stays in memory, for going back in. A
 * guest link is there to be passed on, so it stays, and that page can be reloaded.
 */
function takeCodeOutOfAddress() {
  history.replaceState(history.state, "", location.pathname + location.search);
}

/** The path through a share link: check it, get ready, wait if there is a waiting room, meet, and leave. */
export function JoinFlow() {
  const [code, setCode] = useState(readCode);
  const [stage, setStage] = useState<Stage>({ name: "checking" });
  // Going back in looks at the link afresh: since last time the meeting may have ended, or been locked.
  const [visit, setVisit] = useState(0);
  const [holdingHostCode, setHoldingHostCode] = useState(false);

  useEffect(() => {
    // Taking the code out of the address is not a change of link, so an empty fragment keeps the one in memory.
    const onHash = () => {
      const next = readCode();
      if (next) setCode(next);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Reloading would lose a host code that is only in memory, so the browser asks first.
  useEffect(() => {
    if (!holdingHostCode) return;
    const ask = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", ask);
    return () => window.removeEventListener("beforeunload", ask);
  }, [holdingHostCode]);

  useEffect(() => {
    let live = true;
    setStage({ name: "checking" });
    if (!code) {
      setStage({
        name: "unusable",
        title: TITLES.link_not_found ?? "",
        message: "This address has no meeting code. Check that you copied all of the link. A host link leaves the address once it is open, so if you were hosting, open your host link again: it is on the room's page in the dashboard.",
      });
      return;
    }
    previewLink(code)
      .then((link) => {
        if (!live) return;
        setHoldingHostCode(link.role === "host");
        if (link.role === "host" && readCode() === code) takeCodeOutOfAddress();
        // A host can start it again. Nobody else can do anything with it until then.
        if (link.ended && link.role !== "host") {
          setStage({ name: "unusable", title: TITLES.meeting_ended ?? "", message: "The host ended this meeting. If it starts again, this link will work." });
          return;
        }
        setStage({ name: "prejoin", link });
      })
      .catch((err: unknown) => {
        if (!live) return;
        const e = err instanceof JoinError ? err : new JoinError("unknown", "Something went wrong. Try again.");
        setStage({ name: "unusable", title: TITLES[e.code] ?? "Could not open this meeting", message: e.message });
      });
    return () => {
      live = false;
    };
  }, [code, visit]);

  switch (stage.name) {
    case "checking":
      return (
        <Centered>
          <p className="text-sm text-ink-2" role="status">
            Checking your link&hellip;
          </p>
        </Centered>
      );
    case "unusable":
      return (
        <Centered>
          <div data-testid="link-unusable">
            <h1 className="text-xl font-semibold">{stage.title}</h1>
            <p className="mt-2 text-sm text-ink-2">{stage.message}</p>
          </div>
        </Centered>
      );
    case "prejoin":
      return (
        <PreJoin
          code={code}
          link={stage.link}
          onJoined={(session, choices) => setStage({ name: "room", link: stage.link, session, choices })}
          onWaiting={(ticket, choices) => setStage({ name: "waiting", link: stage.link, ticket, choices })}
        />
      );
    case "waiting":
      return (
        <WaitingRoom
          ticket={stage.ticket}
          title={meetingTitle(stage.link)}
          onAdmitted={(session) => setStage({ name: "room", link: stage.link, session, choices: stage.choices })}
          onRefused={(err) => setStage({ name: "unusable", title: TITLES[err.code] ?? "Could not join", message: err.message })}
          onLeave={() => setVisit((v) => v + 1)}
        />
      );
    case "room":
      return <MeetingRoom key={stage.session.identity} link={stage.link} session={stage.session} choices={stage.choices} onEnded={(end) => setStage({ name: "ended", link: stage.link, end })} />;
    case "ended":
      return <EndScreen link={stage.link} end={stage.end} onRejoin={() => setVisit((v) => v + 1)} />;
  }
}
