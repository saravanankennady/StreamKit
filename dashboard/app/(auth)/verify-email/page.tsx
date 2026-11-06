"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import * as React from "react";
import { AuthHeading } from "@/components/shell/auth-frame";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api } from "@/lib/api";

type State = { kind: "pending" } | { kind: "done" } | { kind: "failed"; message: string };

export default function VerifyEmailPage() {
  const token = useSearchParams().get("token") ?? "";
  const [state, setState] = React.useState<State>(token ? { kind: "pending" } : { kind: "failed", message: "This link has no token in it. Open the link from your email again." });
  // A link works once, and development mode runs effects twice.
  const sent = React.useRef(false);

  React.useEffect(() => {
    if (!token || sent.current) return;
    sent.current = true;
    api("/auth/verify-email", { method: "POST", body: { token } })
      .then(() => setState({ kind: "done" }))
      .catch((err: Error) => setState({ kind: "failed", message: err.message }));
  }, [token]);

  return (
    <>
      <AuthHeading title="Confirm your email" />
      {state.kind === "pending" && (
        <div role="status" aria-label="Confirming" className="flex flex-col gap-2">
          <Skeleton className="w-2/3" />
        </div>
      )}
      {state.kind === "done" && (
        <>
          <Callout tone="good" title="Your email address is confirmed" />
          <Button asChild variant="primary" size="lg" className="mt-4 w-full">
            <Link href="/overview">Continue to the console</Link>
          </Button>
        </>
      )}
      {state.kind === "failed" && (
        <>
          <Callout tone="critical" title="That link did not work">
            {state.message}
          </Callout>
          <p className="mt-4 text-sm text-ink-2">
            Sign in and use the banner at the top of the console to get a new one.{" "}
            <Link className="link" href="/login">
              Sign in
            </Link>
          </p>
        </>
      )}
    </>
  );
}
