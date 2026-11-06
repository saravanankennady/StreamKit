"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { StartMeeting } from "@/components/start-meeting";
import { Button, Centered } from "@/components/ui";

const JOIN_MARK = "/meet/join#";

/** Where a meeting begins: start one with an account, or join one with a whole link. */
export default function MeetingHome() {
  const router = useRouter();
  const [link, setLink] = useState("");
  const [error, setError] = useState<string | null>(null);

  return (
    <Centered>
      <StartMeeting />
      <hr className="my-6 border-line" />
      <h2 className="text-xl font-semibold">Join a meeting</h2>
      <p className="mt-2 text-sm text-ink-2">Meetings are joined with a link from the person who set them up. Paste yours here.</p>
      <form
        className="mt-4 flex flex-col gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          const at = link.indexOf(JOIN_MARK);
          const code = at < 0 ? "" : link.slice(at + JOIN_MARK.length).trim();
          if (!code) {
            setError("That is not a meeting link. Check that you copied all of it.");
            return;
          }
          router.push(`/join#${code}`);
        }}
      >
        <label htmlFor="meeting-link" className="text-sm font-medium">
          Meeting link
        </label>
        <input
          id="meeting-link"
          value={link}
          onChange={(e) => {
            setLink(e.target.value);
            setError(null);
          }}
          className="h-10 rounded-md border border-line-2 bg-surface px-3 text-sm"
          autoComplete="off"
          aria-invalid={error !== null}
          aria-describedby={error ? "meeting-link-error" : undefined}
        />
        {error && (
          <p id="meeting-link-error" role="alert" className="text-sm text-critical-text">
            {error}
          </p>
        )}
        <Button type="submit" variant="primary" className="mt-1">
          Continue
        </Button>
      </form>
    </Centered>
  );
}
