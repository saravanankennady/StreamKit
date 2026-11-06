"use client";

import { Activity, ArrowRight, Boxes, KeyRound, Plus, Ticket, Webhook } from "lucide-react";
import Link from "next/link";
import * as React from "react";
import { Chart } from "@/components/charts/chart";
import { EmptyState, ErrorState, Page, Panel } from "@/components/console/page";
import { CreateRoomDialog } from "@/components/console/room-dialogs";
import { useWorkers } from "@/components/console/workers";
import { Meter, NotMeasured, Sparkline, Tile, TileGrid } from "@/components/console/tile";
import { Callout, StatusBadge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyValue } from "@/components/ui/copy";
import { Skeleton, TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { ApiError, isForbidden, isRouteMissing } from "@/lib/api";
import { formatBitrate, formatBytes, formatCount, formatDecimal, formatDuration, formatMinutes, formatMinutesTick, formatRelative, formatShortTime } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useLive, type LiveStatus } from "@/lib/live";
import { useProject, useProjectQuery } from "@/lib/session";
import type { AnalyticsOverview, AuditEntry, Items, LogEvent, Page as ApiPage, ProjectSnapshot, RoomRecord, UsagePoint } from "@/lib/types";

const DAY = 86_400_000;

function verdict(status: LiveStatus, s: ProjectSnapshot | null, age: number | null): { tone: Tone; title: string; detail: string } {
  if (!s) return { tone: "neutral", title: "Waiting for the first reading", detail: "The server sends one every second." };
  if (status === "live" && age !== null && age > 10) return { tone: "serious", title: `No reading for ${age} seconds`, detail: "The connection is open, but the server has stopped sending. It may be overloaded." };
  const load = `${s.active_rooms} ${s.active_rooms === 1 ? "room" : "rooms"} open, ${s.participants} ${s.participants === 1 ? "person" : "people"} connected.`;
  if (status === "offline") return { tone: "critical", title: "The server stopped answering", detail: "These figures are the last ones received, and may be out of date." };
  if (!s.node.healthy) return { tone: "critical", title: `${s.node.node_id} reports itself unhealthy`, detail: load };
  if (s.node.draining) return { tone: "warn", title: `${s.node.node_id} is draining`, detail: `It takes no new sessions until someone resumes it on the Servers page. ${load}` };
  if (s.packet_loss_ratio >= 0.05) return { tone: "serious", title: `Packet loss is ${(s.packet_loss_ratio * 100).toFixed(1)}%`, detail: `Someone is having a bad call. ${load}` };
  if (status === "polling") return { tone: "warn", title: "Working, but the live feed is down", detail: `Figures update every 3 seconds instead of every second. ${load}` };
  return { tone: "good", title: "All good", detail: `${s.node.node_id} is accepting sessions. ${load}` };
}

function LiveTiles({ projects, minutes, bytesIn, bytesOut, pending }: { projects: number; minutes: number | null; bytesIn: number | null; bytesOut: number | null; pending: boolean }) {
  const snapshot = useLive((s) => s.snapshot);
  const history = useLive((s) => s.history);
  const last60 = history.slice(-60);
  const s = snapshot;
  const memPercent = s?.node.memory_total ? (s.node.memory_used / s.node.memory_total) * 100 : 0;
  const live = (v: React.ReactNode) => (s ? v : <Skeleton className="h-6 w-16" />);
  return (
    <TileGrid className="grid-cols-2 md:grid-cols-3 xl:grid-cols-6">
      <Tile label="Projects" value={projects} testId="tile-projects" detail="You can reach" />
      <Tile label="Active rooms" value={live(s?.active_rooms)} testId="tile-rooms" detail={s ? `${s.audio_streams} audio, ${s.video_streams} video streams` : undefined} />
      <Tile label="Participants" value={live(s?.participants)} testId="tile-participants">
        <Sparkline values={last60.map((x) => x.participants)} />
      </Tile>
      <Tile label="Connections" value={live(s?.connections)} testId="tile-connections" detail={s ? `${s.subscriptions} subscriptions` : undefined} />
      <Tile
        label="Streaming minutes"
        value={pending ? <Skeleton className="h-6 w-16" /> : minutes === null ? <NotMeasured>Unavailable</NotMeasured> : formatDecimal(minutes, minutes < 10 ? 1 : 0)}
        testId="tile-minutes"
        detail="Participant minutes, 30 days"
      />
      <Tile
        label="Data transferred"
        value={pending ? <Skeleton className="h-6 w-16" /> : bytesIn === null ? <NotMeasured>Unavailable</NotMeasured> : formatBytes(bytesIn + (bytesOut ?? 0))}
        testId="tile-data"
        detail="In and out, 30 days"
      />
      <Tile label="Bandwidth in" value={live(s && formatBitrate(s.bitrate_in))} testId="tile-bitrate-in">
        <Sparkline values={last60.map((x) => x.bitrate_in)} />
      </Tile>
      <Tile label="Bandwidth out" value={live(s && formatBitrate(s.bitrate_out))} testId="tile-bitrate-out">
        <Sparkline values={last60.map((x) => x.bitrate_out)} />
      </Tile>
      <Tile
        label="Latency"
        value={live(s && (s.avg_rtt_ms > 0 ? `${formatDecimal(s.avg_rtt_ms)} ms` : <NotMeasured />))}
        testId="tile-latency"
        detail={s && s.avg_rtt_ms === 0 ? "Measured once someone subscribes" : "Average round trip"}
      />
      <Tile label="Packet loss" value={live(s && `${(s.packet_loss_ratio * 100).toFixed(1)}%`)} testId="tile-loss" detail="Worst participant" />
      <Tile label="Server CPU" value={live(s && `${s.node.cpu_percent.toFixed(0)}%`)} testId="tile-cpu" detail={s ? `${s.node.num_cpu} cores on ${s.node.hostname}` : undefined}>
        {s && <Meter percent={s.node.cpu_percent} label="Server CPU" />}
      </Tile>
      <Tile label="Server memory" value={live(s && `${memPercent.toFixed(0)}%`)} testId="tile-memory" detail={s ? `${formatBytes(s.node.memory_used)} of ${formatBytes(s.node.memory_total)}` : undefined}>
        {s && <Meter percent={memPercent} label="Server memory" />}
      </Tile>
    </TileGrid>
  );
}

function UsageChart({ usage }: { usage: { data?: Items<UsagePoint>; error: ApiError | null; isPending: boolean; refetch: () => unknown } }) {
  if (usage.isPending) return <Skeleton className="h-52 rounded-none" />;
  if (usage.error) return <ErrorState error={usage.error} onRetry={usage.refetch} />;
  const byDay = new Map(usage.data?.items.map((p) => [new Date(p.bucket).toDateString(), p]));
  const rows = Array.from({ length: 14 }, (_, i) => {
    const d = new Date(Date.now() - (13 - i) * DAY);
    const p = byDay.get(d.toDateString());
    return { x: d.getTime(), participant_minutes: p ? p.participant_minutes : null, peak_participants: p ? p.peak_participants : null };
  });
  return (
    <div className="grid gap-px bg-line md:grid-cols-2">
      <Chart
        title="Streaming minutes"
        description="Participant minutes per day, last 14 days"
        data={rows}
        series={[{ key: "participant_minutes", label: "Participant minutes", slot: 1 }]}
        kind="bar"
        formatX={(x) => new Date(x).toLocaleDateString(undefined, { month: "short", day: "numeric" })}
        formatY={formatMinutesTick}
        formatValue={formatMinutes}
        testId="chart-minutes"
        empty="No usage yet. It is written every 10 seconds while a room is open."
      />
      <Chart
        title="Peak participants"
        description="Most people connected at once, per day"
        data={rows}
        series={[{ key: "peak_participants", label: "Peak participants", slot: 1 }]}
        kind="bar"
        integer
        formatX={(x) => new Date(x).toLocaleDateString(undefined, { month: "short", day: "numeric" })}
        formatY={formatCount}
        empty="Nobody has connected in the last 14 days."
      />
    </div>
  );
}

function SystemStatus() {
  const snapshot = useLive((s) => s.snapshot);
  const status = useLive((s) => s.status);
  const egress = useProjectQuery<unknown>(["media", "egress", "probe"], "/egress?limit=1", { refetchInterval: 60_000 });
  const ingress = useProjectQuery<unknown>(["media", "ingress", "probe"], "/ingress", { refetchInterval: 60_000 });
  const egressWorkers = useWorkers("egress_worker");
  const ingressWorkers = useWorkers("ingress_worker");
  const media = (q: { error: unknown; isPending: boolean }, workers: { known: boolean; count: number }) =>
    q.isPending || !workers.known
      ? { tone: "neutral" as Tone, value: "Checking" }
      : isRouteMissing(q.error)
        ? { tone: "neutral" as Tone, value: "Not running" }
        : q.error
          ? { tone: "serious" as Tone, value: "Not answering" }
          : workers.count === 0
            ? { tone: "warn" as Tone, value: "No worker" }
            : { tone: "good" as Tone, value: `${workers.count} ${workers.count === 1 ? "worker" : "workers"}` };
  const node = snapshot?.node;
  const rows: { label: string; tone: Tone; value: string; detail?: string }[] = [
    {
      label: "Live feed",
      tone: status === "live" ? "good" : status === "polling" ? "warn" : status === "offline" ? "critical" : "neutral",
      value: status === "live" ? "Connected" : status === "polling" ? "Polling over HTTP" : status === "offline" ? "No data" : "Connecting",
    },
    {
      label: "Media server",
      tone: !node ? "neutral" : !node.healthy ? "critical" : node.draining ? "warn" : "good",
      value: !node ? "Waiting" : !node.healthy ? "Unhealthy" : node.draining ? "Draining" : "Accepting sessions",
      detail: node ? `Up ${formatDuration(node.uptime_seconds)}` : undefined,
    },
    { label: "Recording and egress", ...media(egress, egressWorkers) },
    { label: "Ingress", ...media(ingress, ingressWorkers) },
  ];
  return (
    <Panel
      title="System status"
      actions={
        <Link href="/servers" className="link text-xs text-ink-2">
          Servers
        </Link>
      }
    >
      <ul>
        {rows.map((r) => (
          <li key={r.label} className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b px-3 py-2 last:border-b-0">
            <span className="text-sm">{r.label}</span>
            <span className="flex items-center gap-2">
              {r.detail && <span className="text-xs text-ink-3">{r.detail}</span>}
              <StatusBadge tone={r.tone}>{r.value}</StatusBadge>
            </span>
          </li>
        ))}
      </ul>
    </Panel>
  );
}

function QuickActions({ onCreateRoom }: { onCreateRoom: () => void }) {
  const { project } = useProject();
  const linkClass = "flex items-center gap-2 border-b px-3 py-2 text-sm hover:bg-sunken last:border-b-0";
  return (
    <Panel title="Quick actions">
      <div className="border-b px-3 py-2">
        <div className="text-xs text-ink-3">Clients connect to</div>
        <CopyValue value={project.url} className="text-sm" />
      </div>
      <div className="flex flex-col">
        <button type="button" onClick={onCreateRoom} className={`${linkClass} text-left`}>
          <Plus className="size-3.5 text-ink-3" aria-hidden /> Create a room
        </button>
        <Link href="/developer?tab=tokens" className={linkClass}>
          <Ticket className="size-3.5 text-ink-3" aria-hidden /> Generate a token
        </Link>
        <Link href="/developer" className={linkClass}>
          <KeyRound className="size-3.5 text-ink-3" aria-hidden /> Manage API keys
        </Link>
        <Link href="/webhooks" className={linkClass}>
          <Webhook className="size-3.5 text-ink-3" aria-hidden /> Add a webhook
        </Link>
        <Link href="/monitor" className={linkClass}>
          <Activity className="size-3.5 text-ink-3" aria-hidden /> Open the live monitor
        </Link>
      </div>
    </Panel>
  );
}

function RecentRooms() {
  const now = useNow(10_000);
  const rooms = useProjectQuery<ApiPage<RoomRecord>>(["rooms", "history", "recent"], "/rooms/history?limit=6", { refetchInterval: 15_000 });
  return (
    <Panel
      title="Recent rooms"
      actions={
        <Link href="/rooms?tab=history" className="link inline-flex items-center gap-1 text-xs text-ink-2">
          History <ArrowRight className="size-3" aria-hidden />
        </Link>
      }
    >
      {rooms.isPending ? (
        <TableSkeleton rows={3} />
      ) : rooms.error ? (
        <ErrorState error={rooms.error} onRetry={() => rooms.refetch()} />
      ) : rooms.data.items.length === 0 ? (
        <EmptyState icon={Boxes} title="No rooms yet" className="py-6">
          <p>Create one, or let it open when the first person joins.</p>
        </EmptyState>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              <Th>Started</Th>
              <Th>Duration</Th>
              <Th className="text-right">Peak</Th>
            </tr>
          </thead>
          <tbody>
            {rooms.data.items.map((r) => (
              <Tr key={r.sid} data-testid="recent-room">
                <Td className="max-w-44 truncate">
                  {r.ended_at ? (
                    r.name
                  ) : (
                    <Link className="link" href={`/rooms/${encodeURIComponent(r.name)}`}>
                      {r.name}
                    </Link>
                  )}
                </Td>
                <Td className="whitespace-nowrap text-ink-2">{formatRelative(r.created_at, now)}</Td>
                <Td className="num whitespace-nowrap">{r.ended_at ? formatDuration((Date.parse(r.ended_at) - Date.parse(r.created_at)) / 1000) : <StatusBadge tone="good">Open</StatusBadge>}</Td>
                <Td className="num text-right">{r.peak_participants}</Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

function RecentEvents() {
  const now = useNow(10_000);
  const events = useProjectQuery<ApiPage<LogEvent>>(["logs", "recent"], "/logs?limit=8", { refetchInterval: 10_000 });
  return (
    <Panel
      title="Recent events"
      actions={
        <Link href="/logs" className="link inline-flex items-center gap-1 text-xs text-ink-2">
          All logs <ArrowRight className="size-3" aria-hidden />
        </Link>
      }
    >
      {events.isPending ? (
        <TableSkeleton rows={3} />
      ) : events.error ? (
        <ErrorState error={events.error} onRetry={() => events.refetch()} />
      ) : events.data.items.length === 0 ? (
        <EmptyState title="Nothing has happened yet" className="py-6">
          <p>Rooms starting and people joining will show here.</p>
        </EmptyState>
      ) : (
        <ul>
          {events.data.items.map((e) => (
            <li key={e.id} className="flex items-baseline gap-2 border-b px-3 py-1.5 text-sm last:border-b-0" data-testid="recent-event">
              <span className="num w-16 shrink-0 text-xs text-ink-3">{formatRelative(e.created_at, now)}</span>
              <span className="min-w-0 flex-1 truncate" title={e.message}>
                {e.level !== "info" && <StatusBadge tone={e.level === "error" ? "critical" : "warn"} className="mr-1.5">{e.level === "error" ? "Error" : "Warning"}</StatusBadge>}
                {e.message}
              </span>
              {e.room_name && <span className="hidden max-w-32 shrink-0 truncate text-xs text-ink-3 sm:inline">{e.room_name}</span>}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function RecentActivity() {
  const { canAdmin } = useProject();
  const now = useNow(10_000);
  const audit = useProjectQuery<ApiPage<AuditEntry>>(["audit", "recent"], "/audit?limit=6", { refetchInterval: 20_000, enabled: canAdmin });
  if (!canAdmin) return null;
  return (
    <Panel
      title="Activity"
      description="What people did in this project."
      actions={
        <Link href="/logs?tab=audit" className="link inline-flex items-center gap-1 text-xs text-ink-2">
          Audit log <ArrowRight className="size-3" aria-hidden />
        </Link>
      }
    >
      {audit.isPending ? (
        <TableSkeleton rows={3} />
      ) : audit.error ? (
        isForbidden(audit.error) ? null : <ErrorState error={audit.error} onRetry={() => audit.refetch()} />
      ) : audit.data.items.length === 0 ? (
        <EmptyState title="No activity yet" className="py-6" />
      ) : (
        <ul>
          {audit.data.items.map((a) => (
            <li key={a.id} className="flex items-baseline gap-2 border-b px-3 py-1.5 text-sm last:border-b-0">
              <span className="num w-16 shrink-0 text-xs text-ink-3" title={formatShortTime(a.created_at)}>
                {formatRelative(a.created_at, now)}
              </span>
              <span className="mono min-w-0 flex-1 truncate text-xs" title={a.user_email ?? "System"}>{a.action}</span>
              <span className="hidden max-w-40 shrink-0 truncate text-xs text-ink-3 2xl:inline">{a.user_email ?? "System"}</span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

export default function OverviewPage() {
  const { me, project } = useProject();
  const [creating, setCreating] = React.useState(false);
  const status = useLive((s) => s.status);
  const snapshot = useLive((s) => s.snapshot);
  const now = useNow(1000);
  const lastSampleAt = useLive((s) => s.lastSampleAt);
  const overview = useProjectQuery<AnalyticsOverview>(["analytics", "overview", "30d"], "/analytics/overview", { refetchInterval: 30_000 });
  const usage = useProjectQuery<Items<UsagePoint>>(
    ["analytics", "usage", "overview-14d"],
    `/analytics/usage?interval=day&from=${encodeURIComponent(new Date(Date.now() - 14 * DAY).toISOString())}`,
    { refetchInterval: 60_000 },
  );
  const age = lastSampleAt ? Math.max(0, Math.floor((now - lastSampleAt) / 1000)) : null;
  const v = verdict(status, snapshot, age);
  return (
    <Page title="Overview" description={`${project.name}, right now.`}>
      <Callout
        tone={v.tone}
        title={<span data-testid="overview-verdict">{v.title}</span>}
        action={age !== null ? <span className="num text-xs text-ink-3">{age === 0 ? "Just updated" : `Updated ${age}s ago`}</span> : undefined}
      >
        {v.detail}
      </Callout>
      <LiveTiles
        projects={me.projects.length}
        pending={overview.isPending}
        minutes={overview.data ? overview.data.totals.participant_minutes : null}
        bytesIn={overview.data ? overview.data.totals.bytes_in : null}
        bytesOut={overview.data ? overview.data.totals.bytes_out : null}
      />
      <div className="grid gap-4 xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <div className="flex min-w-0 flex-col gap-4">
          <Panel>
            <UsageChart usage={usage} />
          </Panel>
          <div className="grid gap-4 lg:grid-cols-2">
            <RecentRooms />
            <RecentEvents />
          </div>
        </div>
        <div className="flex min-w-0 flex-col gap-4">
          <SystemStatus />
          <QuickActions onCreateRoom={() => setCreating(true)} />
          <RecentActivity />
        </div>
      </div>
      <CreateRoomDialog open={creating} onOpenChange={setCreating} />
    </Page>
  );
}
