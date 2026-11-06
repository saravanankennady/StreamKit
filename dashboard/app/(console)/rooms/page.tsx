"use client";

import { useInfiniteQuery, useMutation } from "@tanstack/react-query";
import { Link2, MoreHorizontal, Plus } from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import * as React from "react";
import { EmptyState, ErrorState, Page, Panel } from "@/components/console/page";
import { CloseRoomDialog, CreateRoomDialog, EditMetadataDialog } from "@/components/console/room-dialogs";
import { ShareRoomDialog } from "@/components/console/share-links";
import { RoomStateBadge } from "@/components/console/status";
import { StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeBlock, CopyValue } from "@/components/ui/copy";
import { Dropdown, DropdownContent, DropdownItem, DropdownTrigger } from "@/components/ui/dropdown";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, ApiError, httpOrigin } from "@/lib/api";
import { formatDateTime, formatDuration, formatShortTime } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { Items, Page as ApiPage, RoomRecord, RoomStat } from "@/lib/types";

function roomSnippet(url: string, token: string | null) {
  return `curl -X POST ${httpOrigin(url)}/api/v1/rooms \\
  -H "Authorization: Bearer ${token ?? "$STREAMKIT_TOKEN"}" \\
  -H "Content-Type: application/json" \\
  -d '{"name": "my-room"}'`;
}

function RoomsEmpty({ onCreate }: { onCreate: () => void }) {
  const { project } = useProject();
  const call = useProjectApi();
  const mint = useMutation<{ token: string }, ApiError>({
    mutationFn: () =>
      call<{ token: string }>("/tokens", {
        method: "POST",
        body: { identity: "room-creator", room: "my-room", room_create: true, can_publish: false, can_subscribe: false, can_publish_data: false, ttl_seconds: 3600 },
      }),
  });
  return (
    <EmptyState
      title="No rooms are open"
      action={
        <Button variant="primary" onClick={onCreate}>
          <Plus aria-hidden /> Create a room
        </Button>
      }
    >
      <p>
        A room opens when you create one, or by itself when the first person joins with a token for it. To create one from your backend, send an access token that has the{" "}
        <code className="mono">room_create</code> grant:
      </p>
      <CodeBlock code={roomSnippet(project.url, mint.data?.token ?? null)} label={mint.data ? "Ready to run: the token below lasts one hour" : `For ${project.name}`} className="w-full" />
      {!mint.data && (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" pending={mint.isPending} onClick={() => mint.mutate()}>
            Fill in a token that lasts one hour
          </Button>
          <span className="text-xs text-ink-3">It can create rooms, and nothing else.</span>
        </div>
      )}
      {mint.error?.code === "no_api_key" ? (
        <p role="alert" className="text-critical-text">
          Tokens are signed with an API key, and this project has none.{" "}
          <Link className="link" href="/developer">
            Create an API key
          </Link>
          , then come back.
        </p>
      ) : mint.error ? (
        <p role="alert" className="text-critical-text">
          {mint.error.message}
        </p>
      ) : null}
    </EmptyState>
  );
}

function RoomRow({ room, now }: { room: RoomStat; now: number }) {
  const [menu, setMenu] = React.useState<"metadata" | "close" | "share" | null>(null);
  const href = `/rooms/${encodeURIComponent(room.name)}`;
  return (
    <Tr data-testid="room-row" data-room={room.name}>
      <Td className="max-w-56">
        <Link className="link block truncate font-medium" href={href}>
          {room.name}
        </Link>
      </Td>
      <Td className="hidden lg:table-cell">
        <CopyValue value={room.sid} className="text-xs text-ink-2" />
      </Td>
      <Td>
        <RoomStateBadge state={room.state} />
      </Td>
      <Td className="num whitespace-nowrap">
        {room.participants}
        <span className="text-ink-3"> / {room.max_participants || "no limit"}</span>
      </Td>
      <Td className="hidden whitespace-nowrap text-ink-2 md:table-cell" title={formatDateTime(room.created_at)}>
        {formatShortTime(room.created_at)}
      </Td>
      <Td className="num hidden whitespace-nowrap sm:table-cell">{formatDuration(Math.max(0, now / 1000 - room.created_at))}</Td>
      <Td className="mono hidden max-w-48 truncate text-xs text-ink-2 xl:table-cell" title={room.metadata}>
        {room.metadata || <span className="text-ink-3">–</span>}
      </Td>
      <Td className="w-8 px-1 text-right">
        <Dropdown>
          <DropdownTrigger asChild>
            <Button variant="ghost" size="iconSm" aria-label={`Actions for ${room.name}`}>
              <MoreHorizontal aria-hidden />
            </Button>
          </DropdownTrigger>
          <DropdownContent>
            <DropdownItem asChild>
              <Link href={href}>Inspect participants</Link>
            </DropdownItem>
            <DropdownItem onSelect={() => setMenu("share")}>Share a link</DropdownItem>
            <DropdownItem onSelect={() => setMenu("metadata")}>Edit metadata</DropdownItem>
            <DropdownItem className="text-critical-text" onSelect={() => setMenu("close")}>
              Close room
            </DropdownItem>
          </DropdownContent>
        </Dropdown>
        <ShareRoomDialog room={room.name} open={menu === "share"} onOpenChange={(o) => setMenu(o ? "share" : null)} />
        <EditMetadataDialog room={room} open={menu === "metadata"} onOpenChange={(o) => setMenu(o ? "metadata" : null)} />
        <CloseRoomDialog room={room} open={menu === "close"} onOpenChange={(o) => setMenu(o ? "close" : null)} />
      </Td>
    </Tr>
  );
}

function OpenRooms({ onCreate }: { onCreate: () => void }) {
  const now = useNow(1000);
  const rooms = useProjectQuery<Items<RoomStat>>(["rooms", "open"], "/rooms", { refetchInterval: 3000 });
  if (rooms.isPending) return <TableSkeleton />;
  if (rooms.error) return <ErrorState error={rooms.error} onRetry={() => rooms.refetch()} />;
  if (rooms.data.items.length === 0) return <RoomsEmpty onCreate={onCreate} />;
  return (
    <Table>
      <thead>
        <tr>
          <Th>Name</Th>
          <Th className="hidden lg:table-cell">SID</Th>
          <Th>Status</Th>
          <Th>Participants</Th>
          <Th className="hidden md:table-cell">Created</Th>
          <Th className="hidden sm:table-cell">Open for</Th>
          <Th className="hidden xl:table-cell">Metadata</Th>
          <Th className="w-8">
            <span className="sr-only">Actions</span>
          </Th>
        </tr>
      </thead>
      <tbody>
        {rooms.data.items.map((room) => (
          <RoomRow key={room.sid} room={room} now={now} />
        ))}
      </tbody>
    </Table>
  );
}

function RoomHistory() {
  const { projectId } = useProject();
  const history = useInfiniteQuery<ApiPage<RoomRecord>, ApiError>({
    queryKey: [projectId, "rooms", "history"],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) => {
      const q = new URLSearchParams({ limit: "50" });
      if (pageParam) q.set("cursor", pageParam as string);
      return api<ApiPage<RoomRecord>>(`/rooms/history?${q}`, { project: projectId, signal });
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
    refetchInterval: 10000,
  });
  if (history.isPending) return <TableSkeleton />;
  if (history.error) return <ErrorState error={history.error} onRetry={() => history.refetch()} />;
  const items = history.data.pages.flatMap((p) => p.items);
  if (items.length === 0) {
    return (
      <EmptyState title="No room history yet">
        <p>Every room session is recorded here when it starts, and stays after it closes, with its peak and total participants.</p>
      </EmptyState>
    );
  }
  return (
    <>
      <Table>
        <thead>
          <tr>
            <Th>Name</Th>
            <Th className="hidden lg:table-cell">SID</Th>
            <Th>Started</Th>
            <Th>Duration</Th>
            <Th className="text-right">Peak</Th>
            <Th className="text-right">Total</Th>
            <Th className="hidden md:table-cell">Node</Th>
            <Th className="hidden xl:table-cell">Metadata</Th>
          </tr>
        </thead>
        <tbody>
          {items.map((r) => (
            <Tr key={r.sid} data-testid="history-row" data-room={r.name}>
              <Td className="max-w-56 truncate font-medium">{r.name}</Td>
              <Td className="hidden lg:table-cell">
                <CopyValue value={r.sid} className="text-xs text-ink-2" />
              </Td>
              <Td className="whitespace-nowrap text-ink-2" title={formatDateTime(r.created_at)}>
                {formatShortTime(r.created_at)}
              </Td>
              <Td className="num whitespace-nowrap">
                {r.ended_at ? formatDuration((Date.parse(r.ended_at) - Date.parse(r.created_at)) / 1000) : <StatusBadge tone="good">Still open</StatusBadge>}
              </Td>
              <Td className="num text-right">{r.peak_participants}</Td>
              <Td className="num text-right">{r.total_participants}</Td>
              <Td className="mono hidden text-xs text-ink-2 md:table-cell">{r.node_id}</Td>
              <Td className="mono hidden max-w-48 truncate text-xs text-ink-2 xl:table-cell" title={r.metadata}>
                {r.metadata || <span className="text-ink-3">–</span>}
              </Td>
            </Tr>
          ))}
        </tbody>
      </Table>
      {history.hasNextPage && (
        <div className="p-3">
          <Button size="sm" pending={history.isFetchingNextPage} onClick={() => history.fetchNextPage()}>
            Load older rooms
          </Button>
        </div>
      )}
    </>
  );
}

export default function RoomsPage() {
  const router = useRouter();
  const params = useSearchParams();
  const tab = params.get("tab") === "history" ? "history" : "open";
  const [creating, setCreating] = React.useState(params.get("new") === "1");
  const [sharing, setSharing] = React.useState(false);
  return (
    <Page
      title="Rooms"
      description="A room is where participants meet. Open rooms are on this server now. History keeps every room session."
      actions={
        <>
          <Button data-testid="share-new" onClick={() => setSharing(true)}>
            <Link2 aria-hidden /> Share a link
          </Button>
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus aria-hidden /> Create room
          </Button>
        </>
      }
    >
      <Tabs value={tab} onValueChange={(v) => router.replace(v === "history" ? "/rooms?tab=history" : "/rooms")}>
        <TabsList aria-label="Rooms">
          <TabsTrigger value="open">Open</TabsTrigger>
          <TabsTrigger value="history">History</TabsTrigger>
        </TabsList>
        <TabsContent value="open" className="mt-4">
          <Panel>
            <OpenRooms onCreate={() => setCreating(true)} />
          </Panel>
        </TabsContent>
        <TabsContent value="history" className="mt-4">
          <Panel>
            <RoomHistory />
          </Panel>
        </TabsContent>
      </Tabs>
      <CreateRoomDialog open={creating} onOpenChange={setCreating} />
      <ShareRoomDialog open={sharing} onOpenChange={setSharing} />
    </Page>
  );
}
