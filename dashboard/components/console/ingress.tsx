"use client";

import * as React from "react";
import { Callout, StatusBadge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeBlock } from "@/components/ui/copy";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { CheckRow } from "@/components/ui/switch";
import type { Ingress, IngressStatus } from "@/lib/types";

const statuses: Record<IngressStatus, { tone: Tone; label: string }> = {
  inactive: { tone: "neutral", label: "Not streaming" },
  buffering: { tone: "warn", label: "Buffering" },
  publishing: { tone: "good", label: "Publishing" },
  error: { tone: "critical", label: "Error" },
};

export function IngressStatusBadge({ status }: { status: IngressStatus }) {
  const s = statuses[status] ?? { tone: "neutral" as const, label: status };
  return <StatusBadge tone={s.tone}>{s.label}</StatusBadge>;
}

export const INPUT_LABELS: Record<string, string> = { rtmp: "RTMP", whip: "WHIP", url: "Pull from URL" };

/** A stream key is shown once, when it is created or rotated, like an API secret. */
export function StreamKeyReveal({ ingress, onDone }: { ingress: Ingress | null; onDone: () => void }) {
  const [saved, setSaved] = React.useState(false);
  React.useEffect(() => {
    if (ingress) setSaved(false);
  }, [ingress]);
  const key = ingress?.stream_key ?? "";
  return (
    <Dialog open={!!ingress} onOpenChange={() => undefined}>
      <DialogContent title="Your stream key" closable={false} onEscapeKeyDown={(e) => e.preventDefault()} onInteractOutside={(e) => e.preventDefault()} onPointerDownOutside={(e) => e.preventDefault()}>
        {ingress && (
          <>
            <DialogBody>
              <Callout tone="warn" title="This is the only time you will see the stream key">
                Anyone with it can publish into this room. If you lose it, rotate it to get a new one.
              </Callout>
              {ingress.input_type === "whip" ? (
                <>
                  <CodeBlock label="WHIP endpoint" code={ingress.publish_url} />
                  <CodeBlock label="Send this header" code={`Authorization: Bearer ${key}`} />
                </>
              ) : (
                <>
                  <CodeBlock label="Server, for OBS and other encoders" code={ingress.publish_url} />
                  <CodeBlock label="Stream key" code={key} />
                </>
              )}
              <CheckRow label="I have saved the stream key" checked={saved} onCheckedChange={setSaved} />
            </DialogBody>
            <DialogFooter>
              <Button variant="primary" disabled={!saved} onClick={onDone}>
                Done
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
