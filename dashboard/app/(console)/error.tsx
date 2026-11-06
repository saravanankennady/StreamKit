"use client";

import { Button } from "@/components/ui/button";

/** A bug in a page should not take the shell down with it, and should say what to do. */
export default function ConsoleError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <div role="alert" className="flex flex-col items-start gap-2 p-6">
      <h1 className="text-xl font-semibold tracking-tight">This page hit a problem</h1>
      <p className="max-w-xl text-ink-2">Something went wrong in the dashboard itself, not on your server. Try again. If it keeps happening, reload the page and report this message:</p>
      <pre className="scroll-quiet max-w-full overflow-x-auto rounded-xs border bg-sunken px-3 py-2 font-mono text-xs">{error.message || "No details."}</pre>
      <Button variant="primary" onClick={reset}>
        Try again
      </Button>
    </div>
  );
}
