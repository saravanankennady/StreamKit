"use client";

import { useInfiniteQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, FileText, ShieldAlert } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import * as React from "react";
import { EmptyState, ErrorState, Page, Panel } from "@/components/console/page";
import { StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeBlock } from "@/components/ui/copy";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, ApiError, isForbidden } from "@/lib/api";
import { formatClock, formatDateTime, formatShortTime } from "@/lib/format";
import { useDebounced } from "@/lib/hooks";
import { useProject } from "@/lib/session";
import type { AuditEntry, LogEvent, Page as ApiPage } from "@/lib/types";

const CATEGORIES = ["room", "participant", "track", "signaling", "webrtc", "recording", "api", "application"] as const;
/** Categories the server has a name for, but does not write to yet. An empty result there is not reassurance. */
const NOT_WRITTEN_YET = new Set(["signaling", "webrtc", "application"]);
const LEVELS = ["info", "warn", "error"] as const;

const hasDetails = (d: unknown) => d !== null && typeof d === "object" && Object.keys(d as object).length > 0;

function LevelBadge({ level }: { level: string }) {
  if (level === "error") return <StatusBadge tone="critical">Error</StatusBadge>;
  if (level === "warn") return <StatusBadge tone="warn">Warning</StatusBadge>;
  return <span className="text-xs text-ink-3">Info</span>;
}

function EventRow({ e }: { e: LogEvent }) {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Tr data-testid="log-row" data-category={e.category} data-level={e.level}>
        <Td className="num text-xs whitespace-nowrap text-ink-2" title={formatDateTime(e.created_at)}>
          <span className="hidden sm:inline">{formatShortTime(e.created_at).split(",")[0]} </span>
          {formatClock(e.created_at)}
        </Td>
        <Td>
          <LevelBadge level={e.level} />
        </Td>
        <Td className="hidden text-xs text-ink-2 sm:table-cell">{e.category}</Td>
        <Td className="max-w-md min-w-40">
          <button type="button" aria-expanded={open} className="flex w-full min-w-0 items-center gap-1 text-left" onClick={() => setOpen((v) => !v)}>
            {open ? <ChevronDown className="size-3 shrink-0 text-ink-3" aria-hidden /> : <ChevronRight className="size-3 shrink-0 text-ink-3" aria-hidden />}
            <span className="truncate" data-testid="log-message">
              {e.message}
            </span>
          </button>
        </Td>
        <Td className="hidden max-w-36 truncate text-xs text-ink-2 md:table-cell">{e.room_name || "–"}</Td>
        <Td className="hidden max-w-36 truncate text-xs text-ink-2 lg:table-cell">{e.participant_identity || "–"}</Td>
      </Tr>
      {open && (
        <tr>
          <td colSpan={6} className="border-b bg-sunken p-3">
            <dl className="mb-2 grid grid-cols-[repeat(auto-fill,minmax(10rem,1fr))] gap-x-4 gap-y-2 text-xs">
              {(
                [
                  ["Type", e.type],
                  ["Category", e.category],
                  ["Room", e.room_name],
                  ["Room SID", e.room_sid],
                  ["Participant", e.participant_identity],
                  ["Track SID", e.track_sid],
                  ["Time", formatDateTime(e.created_at)],
                ] as const
              )
                .filter(([, v]) => v)
                .map(([k, v]) => (
                  <div key={k} className="min-w-0">
                    <dt className="text-ink-3">{k}</dt>
                    <dd className="mono break-all">{v}</dd>
                  </div>
                ))}
            </dl>
            {hasDetails(e.details) && <CodeBlock code={JSON.stringify(e.details, null, 2)} />}
          </td>
        </tr>
      )}
    </>
  );
}

function EventLog() {
  const { projectId } = useProject();
  const params = useSearchParams();
  const router = useRouter();
  const [search, setSearch] = React.useState(params.get("q") ?? "");
  const [room, setRoom] = React.useState(params.get("room") ?? "");
  const [category, setCategory] = React.useState(params.get("category") ?? "all");
  const [level, setLevel] = React.useState(params.get("level") ?? "all");
  const [follow, setFollow] = React.useState(true);
  const q = useDebounced(search.trim());
  const roomFilter = useDebounced(room.trim());

  // Keep filters in the address, so a filtered log can be shared in an incident channel.
  React.useEffect(() => {
    const next = new URLSearchParams();
    if (q) next.set("q", q);
    if (roomFilter) next.set("room", roomFilter);
    if (category !== "all") next.set("category", category);
    if (level !== "all") next.set("level", level);
    const qs = next.toString();
    router.replace(qs ? `/logs?${qs}` : "/logs", { scroll: false });
  }, [q, roomFilter, category, level, router]);

  const events = useInfiniteQuery<ApiPage<LogEvent>, ApiError>({
    queryKey: [projectId, "logs", "list", q, roomFilter, category, level],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) => {
      const s = new URLSearchParams({ limit: "50" });
      if (q) s.set("q", q);
      if (roomFilter) s.set("room", roomFilter);
      if (category !== "all") s.set("category", category);
      if (level !== "all") s.set("level", level);
      if (pageParam) s.set("cursor", pageParam as string);
      return api<ApiPage<LogEvent>>(`/logs?${s}`, { project: projectId, signal });
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
    refetchInterval: follow ? 3000 : false,
  });

  const filtered = q || roomFilter || category !== "all" || level !== "all";
  const items = events.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <Input className="w-full sm:w-64" type="search" aria-label="Search logs" placeholder="Search messages and names" value={search} onChange={(e) => setSearch(e.target.value)} />
        <Input className="w-full sm:w-40" aria-label="Room name" placeholder="Exact room name" value={room} onChange={(e) => setRoom(e.target.value)} />
        <Select
          className="w-[calc(50%-0.25rem)] sm:w-40"
          aria-label="Category"
          value={category}
          onValueChange={setCategory}
          options={[{ value: "all", label: "All categories" }, ...CATEGORIES.map((c) => ({ value: c, label: c.charAt(0).toUpperCase() + c.slice(1) }))]}
        />
        <Select
          className="w-[calc(50%-0.25rem)] sm:w-36"
          aria-label="Level"
          value={level}
          onValueChange={setLevel}
          options={[{ value: "all", label: "All levels" }, ...LEVELS.map((c) => ({ value: c, label: c === "warn" ? "Warning" : c.charAt(0).toUpperCase() + c.slice(1) }))]}
        />
        <label className="ml-auto flex items-center gap-2 text-sm text-ink-2">
          <Switch checked={follow} onCheckedChange={setFollow} aria-label="Refresh every 3 seconds" />
          Follow new events
        </label>
      </div>
      <Panel>
        {events.isPending ? (
          <TableSkeleton />
        ) : events.error ? (
          <ErrorState error={events.error} onRetry={() => events.refetch()} />
        ) : items.length === 0 ? (
          category !== "all" && NOT_WRITTEN_YET.has(category) ? (
            <EmptyState icon={FileText} title={`The server does not write ${category} events yet`}>
              <p>This category exists, but nothing fills it in this build, so an empty list here does not mean nothing went wrong. Room, participant, track, recording and API events are recorded today.</p>
            </EmptyState>
          ) : filtered ? (
            <EmptyState icon={FileText} title="No events match these filters">
              <p>Search matches whole words in the message, type, room and participant. Clear a filter to see more.</p>
            </EmptyState>
          ) : (
            <EmptyState icon={FileText} title="No events yet">
              <p>Events appear here when rooms start and finish, people join and leave, tracks are published, and recordings and streams run. They are kept for 30 days.</p>
            </EmptyState>
          )
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>Time</Th>
                  <Th>Level</Th>
                  <Th className="hidden sm:table-cell">Category</Th>
                  <Th>Message</Th>
                  <Th className="hidden md:table-cell">Room</Th>
                  <Th className="hidden lg:table-cell">Participant</Th>
                </tr>
              </thead>
              <tbody>
                {items.map((e) => (
                  <EventRow key={e.id} e={e} />
                ))}
              </tbody>
            </Table>
            {events.hasNextPage && (
              <div className="p-3">
                <Button size="sm" pending={events.isFetchingNextPage} onClick={() => events.fetchNextPage()} data-testid="logs-more">
                  Load older events
                </Button>
              </div>
            )}
          </>
        )}
      </Panel>
    </>
  );
}

function AuditRow({ a }: { a: AuditEntry }) {
  const [open, setOpen] = React.useState(false);
  const expandable = hasDetails(a.details);
  return (
    <>
      <Tr data-testid="audit-row" data-action={a.action}>
        <Td className="num text-xs whitespace-nowrap text-ink-2" title={formatDateTime(a.created_at)}>
          {formatShortTime(a.created_at)}
        </Td>
        <Td>
          {expandable ? (
            <button type="button" aria-expanded={open} className="flex items-center gap-1 text-left" onClick={() => setOpen((v) => !v)}>
              {open ? <ChevronDown className="size-3 shrink-0 text-ink-3" aria-hidden /> : <ChevronRight className="size-3 shrink-0 text-ink-3" aria-hidden />}
              <span className="mono text-xs">{a.action}</span>
            </button>
          ) : (
            <span className="mono pl-4 text-xs">{a.action}</span>
          )}
        </Td>
        <Td className="mono hidden max-w-64 truncate text-xs text-ink-2 sm:table-cell" title={a.target}>
          {a.target || "–"}
        </Td>
        <Td className="max-w-48 truncate text-ink-2">{a.user_email ?? "System"}</Td>
        <Td className="mono hidden text-xs text-ink-2 md:table-cell">{a.ip || "–"}</Td>
      </Tr>
      {open && (
        <tr>
          <td colSpan={5} className="border-b bg-sunken p-3">
            <CodeBlock code={JSON.stringify(a.details, null, 2)} />
          </td>
        </tr>
      )}
    </>
  );
}

function AuditLog() {
  const { projectId, canAdmin } = useProject();
  const audit = useInfiniteQuery<ApiPage<AuditEntry>, ApiError>({
    queryKey: [projectId, "audit", "list"],
    initialPageParam: "",
    enabled: canAdmin,
    queryFn: ({ pageParam, signal }) => {
      const s = new URLSearchParams({ limit: "50" });
      if (pageParam) s.set("cursor", pageParam as string);
      return api<ApiPage<AuditEntry>>(`/audit?${s}`, { project: projectId, signal });
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  if (!canAdmin || isForbidden(audit.error)) {
    return (
      <Panel>
        <EmptyState icon={ShieldAlert} title="Only owners and admins can read the audit log">
          <p>Your role in this organization is member.</p>
        </EmptyState>
      </Panel>
    );
  }
  const items = audit.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Panel title="Audit log" description="Who changed what in this project.">
      {audit.isPending ? (
        <TableSkeleton />
      ) : audit.error ? (
        <ErrorState error={audit.error} onRetry={() => audit.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="Nothing recorded yet">
          <p>Creating rooms, keys and webhooks, and muting or removing people, all land here.</p>
        </EmptyState>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Time</Th>
                <Th>Action</Th>
                <Th className="hidden sm:table-cell">Target</Th>
                <Th>Who</Th>
                <Th className="hidden md:table-cell">Address</Th>
              </tr>
            </thead>
            <tbody>
              {items.map((a) => (
                <AuditRow key={a.id} a={a} />
              ))}
            </tbody>
          </Table>
          {audit.hasNextPage && (
            <div className="p-3">
              <Button size="sm" pending={audit.isFetchingNextPage} onClick={() => audit.fetchNextPage()}>
                Load older entries
              </Button>
            </div>
          )}
        </>
      )}
    </Panel>
  );
}

export default function LogsPage() {
  const params = useSearchParams();
  const router = useRouter();
  const tab = params.get("tab") === "audit" ? "audit" : "events";
  return (
    <Page title="Logs and events" description="What happened in this project, newest first. Event history is kept for 30 days.">
      <Tabs value={tab} onValueChange={(v) => router.replace(v === "audit" ? "/logs?tab=audit" : "/logs")}>
        <TabsList aria-label="Logs">
          <TabsTrigger value="events">Events</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
        </TabsList>
        <TabsContent value="events" className="mt-4 flex flex-col gap-3">
          <EventLog />
        </TabsContent>
        <TabsContent value="audit" className="mt-4">
          <AuditLog />
        </TabsContent>
      </Tabs>
    </Page>
  );
}
