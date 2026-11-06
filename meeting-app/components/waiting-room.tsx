"use client";

import { useEffect, useRef } from "react";
import { checkTicket, JoinError, type JoinSession, leaveWaitingRoom } from "@/lib/join";
import { Button, Centered } from "./ui";

// The server forgets someone who has not asked for 15 seconds, so this has room to miss one or two.
const CHECK_INTERVAL_MS = 2000;

/** Someone waits here until a host decides. They hold no token until then, so there is nothing to work around. */
export function WaitingRoom({ ticket, title, onAdmitted, onRefused, onLeave }: {
  ticket: string;
  title: string;
  onAdmitted: (session: JoinSession) => void;
  onRefused: (error: JoinError) => void;
  onLeave: () => void;
}) {
  const answered = useRef({ onAdmitted, onRefused });
  answered.current = { onAdmitted, onRefused };

  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const check = () => {
      checkTicket(ticket)
        .then((answer) => {
          if (!live) return;
          if (answer.status === "admitted") {
            answered.current.onAdmitted(answer);
            return;
          }
          timer = setTimeout(check, CHECK_INTERVAL_MS);
        })
        .catch((err: unknown) => {
          if (!live) return;
          // A dropped connection is no answer. Only the server saying no is.
          if (err instanceof JoinError && err.code !== "unreachable") {
            answered.current.onRefused(err);
            return;
          }
          timer = setTimeout(check, CHECK_INTERVAL_MS);
        });
    };
    // The first look waits too: a ticket pays out once, and React's development double-mount must not spend it unseen.
    timer = setTimeout(check, CHECK_INTERVAL_MS / 2);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [ticket]);

  return (
    <Centered>
      <div data-testid="waiting-room">
        <p className="text-xs text-ink-3">You are waiting to join</p>
        <h1 className="text-xl font-semibold break-words">{title}</h1>
        <p role="status" className="mt-2 text-sm text-ink-2">
          Someone running the meeting will let you in. Stay on this page.
        </p>
        <Button
          className="mt-5"
          data-testid="waiting-leave"
          onClick={() => {
            leaveWaitingRoom(ticket);
            onLeave();
          }}
        >
          Stop waiting
        </Button>
      </div>
    </Centered>
  );
}
