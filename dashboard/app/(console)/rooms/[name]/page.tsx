"use client";

import { ArrowLeft, Link2, Pencil, X } from "lucide-react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import * as React from "react";
import { EmptyState, ErrorState, Facts, Page, Panel } from "@/components/console/page";
import { ParticipantsTable, UnmuteNote } from "@/components/console/participants";
import { CloseRoomDialog, EditMetadataDialog } from "@/components/console/room-dialogs";
import { ShareLinksPanel, ShareRoomDialog } from "@/components/console/share-links";
import { RoomStateBadge } from "@/components/console/status";
import { Button } from "@/components/ui/button";
import { CodeBlock, CopyValue } from "@/components/ui/copy";
import { Skeleton, TableSkeleton } from "@/components/ui/skeleton";
import { formatBitrate, formatDateTime, formatDuration } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject, useProjectQuery } from "@/lib/session";
import type { Items, ParticipantStat, RoomStat } from "@/lib/types";

export default function RoomPage() {
  const name = decodeURIComponent(useParams<{ name: string }>().name);
  const path = encodeURIComponent(name);
  const router = useRouter();
  const now = useNow(1000);
  const { project } = useProject();
  const [dialog, setDialog] = React.useState<"metadata" | "close" | "share" | null>(null);
  const room = useProjectQuery<RoomStat>(["rooms", "room", name], `/rooms/${path}`, { refetchInterval: 2000 });
  const participants = useProjectQuery<Items<ParticipantStat>>(["rooms", "participants", name], `/participants?room=${path}`, { refetchInterval: 2000, enabled: !room.error });

  const back = (
    <Link href="/rooms" className="link inline-flex items-center gap-1 text-sm text-ink-2">
      <ArrowLeft className="size-3" aria-hidden /> All rooms
    </Link>
  );

  if (room.error) {
    if (room.error.status === 404) {
      return (
        <Page title={name}>
          <Panel>
            <EmptyState
              title="This room is not open"
              action={
                <>
                  <Button asChild>
                    <Link href="/rooms">Open rooms</Link>
                  </Button>
                  <Button asChild variant="ghost">
                    <Link href="/rooms?tab=history">Room history</Link>
                  </Button>
                </>
              }
            >
              <p>It may have closed after its timeout, or been closed by someone. Past sessions are in room history.</p>
            </EmptyState>
          </Panel>
        </Page>
      );
    }
    return (
      <Page title={name}>
        <Panel>
          <ErrorState error={room.error} onRetry={() => room.refetch()} />
        </Panel>
      </Page>
    );
  }

  const r = room.data;
  return (
    <Page
      title={
        <span className="flex flex-wrap items-center gap-2">
          {name} {r && <RoomStateBadge state={r.state} />}
        </span>
      }
      description={back}
      actions={
        r && (
          <>
            <Button data-testid="room-share" onClick={() => setDialog("share")}>
              <Link2 aria-hidden /> Share a link
            </Button>
            <Button onClick={() => setDialog("metadata")}>
              <Pencil aria-hidden /> Edit metadata
            </Button>
            <Button variant="danger" onClick={() => setDialog("close")}>
              <X aria-hidden /> Close room
            </Button>
          </>
        )
      }
    >
      <Panel title="Details">
        {!r ? (
          <div className="p-3">
            <Skeleton className="h-16" />
          </div>
        ) : (
          <>
            <Facts
              items={[
                { label: "SID", value: <CopyValue value={r.sid} /> },
                { label: "Participants", value: r.max_participants ? `${r.participants} of ${r.max_participants}` : `${r.participants}, no limit`, testId: "room-participants" },
                { label: "Publishing", value: `${r.publishers} ${r.publishers === 1 ? "person" : "people"}` },
                { label: "Streams", value: `${r.audio_streams} audio, ${r.video_streams} video` },
                { label: "Bitrate in", value: formatBitrate(r.bitrate_in) },
                { label: "Bitrate out", value: formatBitrate(r.bitrate_out) },
                { label: "Created", value: formatDateTime(r.created_at) },
                { label: "Open for", value: formatDuration(Math.max(0, now / 1000 - r.created_at)) },
                { label: "Empty timeout", value: `${r.empty_timeout} s` },
                { label: "Departure timeout", value: `${r.departure_timeout} s` },
                { label: "Recording", value: r.active_recording ? "In progress" : "Not recording" },
              ]}
            />
            <div className="border-t p-3">
              <div className="text-xs text-ink-3">Metadata</div>
              {r.metadata ? (
                <pre className="scroll-quiet mt-1 overflow-x-auto font-mono text-xs whitespace-pre-wrap" data-testid="room-metadata">
                  {r.metadata}
                </pre>
              ) : (
                <p className="mt-1 text-sm text-ink-3" data-testid="room-metadata">
                  None set.
                </p>
              )}
            </div>
          </>
        )}
      </Panel>
      <Panel title="Participants">
        {participants.isPending ? (
          <TableSkeleton rows={2} />
        ) : participants.error ? (
          <ErrorState error={participants.error} onRetry={() => participants.refetch()} />
        ) : participants.data.items.length === 0 ? (
          <EmptyState title="Nobody is in this room">
            <p>To invite people, share a link. They can join from their browser, with no account.</p>
            <div>
              <Button variant="primary" data-testid="room-empty-share" onClick={() => setDialog("share")}>
                <Link2 aria-hidden /> Share a link
              </Button>
            </div>
            <p>
              Building an app? Mint a token for room <code className="mono">{name}</code> in{" "}
              <Link className="link" href={`/developer?tab=tokens&room=${path}`}>
                Developer settings
              </Link>
              , and have your SDK connect to this address. It does not open in a browser.
            </p>
            <CodeBlock code={project.url} label="For SDKs" />
          </EmptyState>
        ) : (
          <ParticipantsTable participants={participants.data.items} now={now} showRoom={false} />
        )}
      </Panel>
      {participants.data && participants.data.items.length > 0 && <UnmuteNote />}
      {r && <ShareLinksPanel room={name} onCreate={() => setDialog("share")} />}
      {r && (
        <>
          <ShareRoomDialog room={name} open={dialog === "share"} onOpenChange={(o) => setDialog(o ? "share" : null)} />
          <EditMetadataDialog room={r} open={dialog === "metadata"} onOpenChange={(o) => setDialog(o ? "metadata" : null)} />
          <CloseRoomDialog room={r} open={dialog === "close"} onOpenChange={(o) => setDialog(o ? "close" : null)} onClosed={() => router.push("/rooms")} />
        </>
      )}
    </Page>
  );
}
