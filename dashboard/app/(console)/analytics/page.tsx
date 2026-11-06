"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import * as React from "react";
import { Chart, type ChartRow } from "@/components/charts/chart";
import { ErrorState, Page, Panel } from "@/components/console/page";
import { NotMeasured, Tile, TileGrid } from "@/components/console/tile";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { formatBytes, formatBytesTick, formatCount, formatDecimal, formatDuration, formatDurationTick, formatMinutes, formatMinutesTick, formatMsTick, formatPercent } from "@/lib/format";
import { api, ApiError } from "@/lib/api";
import { useProject, useProjectQuery } from "@/lib/session";
import type { AnalyticsOverview, Items, LogEvent, Page as ApiPage, RoomRecord, UsagePoint } from "@/lib/types";

type Interval = "hour" | "day" | "month";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

const PRESETS: { id: string; label: string; ms: number; interval: Interval }[] = [
  { id: "24h", label: "Last 24 hours", ms: DAY, interval: "hour" },
  { id: "7d", label: "Last 7 days", ms: 7 * DAY, interval: "day" },
  { id: "30d", label: "Last 30 days", ms: 30 * DAY, interval: "day" },
  { id: "90d", label: "Last 90 days", ms: 90 * DAY, interval: "day" },
  { id: "365d", label: "Last 12 months", ms: 365 * DAY, interval: "month" },
];

const INTERVALS = [
  { value: "hour", label: "Hourly" },
  { value: "day", label: "Daily" },
  { value: "month", label: "Monthly" },
];

function bucketStart(t: number, interval: Interval): number {
  const d = new Date(t);
  if (interval === "hour") d.setMinutes(0, 0, 0);
  else if (interval === "day") d.setHours(0, 0, 0, 0);
  else {
    d.setDate(1);
    d.setHours(0, 0, 0, 0);
  }
  return d.getTime();
}

function nextBucket(t: number, interval: Interval): number {
  const d = new Date(t);
  if (interval === "hour") d.setHours(d.getHours() + 1);
  else if (interval === "day") d.setDate(d.getDate() + 1);
  else d.setMonth(d.getMonth() + 1);
  return d.getTime();
}

/** A row for every bucket in the range, so time reads evenly. Buckets with no usage hold no value, not a zero. */
function fillBuckets(points: UsagePoint[], from: number, to: number, interval: Interval, map: (p: UsagePoint) => Record<string, number | null>): ChartRow[] {
  const byBucket = new Map(points.map((p) => [bucketStart(Date.parse(p.bucket), interval), p]));
  const rows: ChartRow[] = [];
  let guard = 0;
  for (let t = bucketStart(from, interval); t <= to && guard < 1000; t = nextBucket(t, interval), guard++) {
    const p = byBucket.get(t);
    rows.push({ x: t, ...(p ? map(p) : {}) });
  }
  return rows;
}

function xFormat(interval: Interval) {
  return (x: number | string) => {
    const d = new Date(x);
    if (interval === "hour") return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
    if (interval === "day") return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
    return d.toLocaleDateString(undefined, { month: "short", year: "numeric" });
  };
}

function xLong(interval: Interval) {
  return (x: number | string) => {
    const d = new Date(x);
    if (interval === "hour") return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    if (interval === "day") return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
    return d.toLocaleDateString(undefined, { month: "long", year: "numeric" });
  };
}

function toInput(t: number) {
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** The API reports one average for a whole range. Per-period durations come from room history, newest first. */
const HISTORY_PAGES = 5;
const HISTORY_PAGE_SIZE = 200;

function useFinishedRooms(from: number | undefined, qs: string) {
  const { projectId } = useProject();
  return useQuery<{ rooms: RoomRecord[]; truncated: boolean }, ApiError>({
    queryKey: [projectId, "analytics", "rooms", qs],
    enabled: from !== undefined,
    placeholderData: (prev) => prev,
    queryFn: async ({ signal }) => {
      const rooms: RoomRecord[] = [];
      let cursor = "";
      for (let i = 0; i < HISTORY_PAGES; i++) {
        const q = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
        if (cursor) q.set("cursor", cursor);
        const page = await api<ApiPage<RoomRecord>>(`/rooms/history?${q}`, { project: projectId, signal });
        rooms.push(...page.items);
        const last = page.items[page.items.length - 1];
        if (!page.next_cursor || !last || Date.parse(last.created_at) < (from ?? 0)) return { rooms, truncated: false };
        cursor = page.next_cursor;
      }
      return { rooms, truncated: true };
    },
  });
}

/** The usage table has a field for connection failures, but the server does not fill it. The event log does know. */
function useAbortedConnections(enabled: boolean) {
  const { projectId } = useProject();
  return useQuery<ApiPage<LogEvent>, ApiError>({
    queryKey: [projectId, "analytics", "aborted"],
    enabled,
    queryFn: ({ signal }) => api<ApiPage<LogEvent>>("/logs?q=participant_connection_aborted&limit=200", { project: projectId, signal }),
  });
}

export default function AnalyticsPage() {
  const [preset, setPreset] = React.useState("30d");
  const [interval, setIntervalChoice] = React.useState<Interval>("day");
  const [custom, setCustom] = React.useState(() => ({ from: toInput(Date.now() - 30 * DAY), to: toInput(Date.now()) }));
  // Fixed when the range is chosen, so the query key does not change every render.
  const [now, setNow] = React.useState(() => Date.now());

  const range = React.useMemo(() => {
    if (preset === "custom") {
      const from = Date.parse(`${custom.from}T00:00:00`);
      const to = Date.parse(`${custom.to}T23:59:59`);
      return Number.isFinite(from) && Number.isFinite(to) && from <= to ? { from, to } : null;
    }
    const p = PRESETS.find((x) => x.id === preset);
    return p ? { from: now - p.ms, to: now } : null;
  }, [preset, custom, now]);

  const qs = range ? `from=${encodeURIComponent(new Date(range.from).toISOString())}&to=${encodeURIComponent(new Date(range.to).toISOString())}` : "";
  const enabled = !!range;
  const overview = useProjectQuery<AnalyticsOverview>(["analytics", "overview", qs], `/analytics/overview?${qs}`, { enabled, placeholderData: (prev) => prev });
  const usage = useProjectQuery<Items<UsagePoint>>(["analytics", "usage", interval, qs], `/analytics/usage?interval=${interval}&${qs}`, { enabled, placeholderData: (prev) => prev });

  const history = useFinishedRooms(range?.from, qs);
  const aborted = useAbortedConnections(enabled);

  const fmtX = xFormat(interval);
  const fmtLong = xLong(interval);
  const points = usage.data?.items ?? [];
  const totals = overview.data?.totals;
  const lossRatio = totals && totals.packets_in + totals.packets_lost > 0 ? totals.packets_lost / (totals.packets_in + totals.packets_lost) : null;
  const refetching = (usage.isFetching && !usage.isPending) || (overview.isFetching && !overview.isPending);

  const failures = React.useMemo(() => {
    if (!range) return [];
    return (aborted.data?.items ?? []).filter((e) => e.type === "participant_connection_aborted" && Date.parse(e.created_at) >= range.from && Date.parse(e.created_at) <= range.to);
  }, [aborted.data, range]);

  const rows = React.useMemo(() => {
    if (!range) return [];
    const failuresByBucket = new Map<number, number>();
    for (const e of failures) {
      const key = bucketStart(Date.parse(e.created_at), interval);
      failuresByBucket.set(key, (failuresByBucket.get(key) ?? 0) + 1);
    }
    const durations = new Map<number, { sum: number; n: number }>();
    for (const r of history.data?.rooms ?? []) {
      const start = Date.parse(r.created_at);
      if (!r.ended_at || start < range.from || start > range.to) continue;
      const key = bucketStart(start, interval);
      const d = durations.get(key) ?? { sum: 0, n: 0 };
      d.sum += (Date.parse(r.ended_at) - start) / 1000;
      d.n += 1;
      durations.set(key, d);
    }
    const usage = fillBuckets(points, range.from, range.to, interval, (p) => ({
      peak_participants: p.peak_participants,
      peak_rooms: p.peak_rooms,
      participant_minutes: p.participant_minutes,
      room_minutes: p.room_minutes,
      audio_minutes: p.audio_minutes,
      video_minutes: p.video_minutes,
      recording_minutes: p.recording_minutes,
      bytes_in: p.bytes_in,
      bytes_out: p.bytes_out,
      rtt: p.avg_rtt_ms > 0 ? p.avg_rtt_ms : null,
      loss: p.packets_in + p.packets_lost > 0 ? (p.packets_lost / (p.packets_in + p.packets_lost)) * 100 : null,
    }));
    return usage.map((row) => {
      const d = durations.get(row.x as number);
      return { ...row, failures: failuresByBucket.get(row.x as number) ?? null, avg_duration: d ? d.sum / d.n : null };
    });
  }, [points, range, interval, failures, history.data]);

  const failuresCapped = (aborted.data?.items.length ?? 0) >= 200;

  const emptyHint = "No usage in this range. It is written every 10 seconds while a room is open.";

  return (
    <Page title="Analytics" description="What this project has used, from the usage the server records. Everything below follows the range you pick here.">
      <div className="flex flex-wrap items-end gap-2" role="group" aria-label="What to show">
        <Field label="Range" className="w-44">
          <Select
            value={preset}
            onValueChange={(v) => {
              setPreset(v);
              setNow(Date.now());
              const p = PRESETS.find((x) => x.id === v);
              if (p) setIntervalChoice(p.interval);
            }}
            options={[...PRESETS.map((p) => ({ value: p.id, label: p.label })), { value: "custom", label: "Custom dates" }]}
          />
        </Field>
        {preset === "custom" && (
          <>
            <Field label="From" className="w-40">
              <Input type="date" value={custom.from} max={custom.to} onChange={(e) => setCustom((c) => ({ ...c, from: e.target.value }))} />
            </Field>
            <Field label="To" className="w-40">
              <Input type="date" value={custom.to} min={custom.from} onChange={(e) => setCustom((c) => ({ ...c, to: e.target.value }))} />
            </Field>
          </>
        )}
        <Field label="Group by" className="w-32">
          <Select value={interval} onValueChange={(v) => setIntervalChoice(v as Interval)} options={INTERVALS} />
        </Field>
        <Button onClick={() => setNow(Date.now())} pending={refetching}>
          Refresh
        </Button>
      </div>

      {usage.error || overview.error ? (
        <Panel>
          <ErrorState
            error={usage.error ?? overview.error}
            onRetry={() => {
              void usage.refetch();
              void overview.refetch();
            }}
          />
        </Panel>
      ) : (
        <div className={refetching ? "flex flex-col gap-4 opacity-60" : "flex flex-col gap-4"}>
          <TileGrid className="grid-cols-2 md:grid-cols-3 xl:grid-cols-9 max-md:[&>*:last-child]:col-span-2">
            <Tile label="Rooms" value={overview.data ? formatCount(overview.data.total_rooms) : <Skeleton className="h-6 w-12" />} testId="analytics-rooms" detail="Sessions started" />
            <Tile label="Average room" value={overview.data ? (overview.data.avg_room_seconds > 0 ? formatDuration(overview.data.avg_room_seconds) : <NotMeasured>None finished</NotMeasured>) : <Skeleton className="h-6 w-12" />} detail="Finished rooms" />
            <Tile label="Streaming" value={totals ? formatMinutes(totals.participant_minutes) : <Skeleton className="h-6 w-12" />} testId="analytics-minutes" detail="Participant time" />
            <Tile label="Peak participants" value={totals ? totals.peak_participants : <Skeleton className="h-6 w-12" />} detail="At once" />
            <Tile label="Data in" value={totals ? formatBytes(totals.bytes_in) : <Skeleton className="h-6 w-12" />} testId="analytics-data-in" />
            <Tile label="Data out" value={totals ? formatBytes(totals.bytes_out) : <Skeleton className="h-6 w-12" />} />
            <Tile label="Packet loss" value={totals ? (lossRatio === null ? <NotMeasured>No packets</NotMeasured> : formatPercent(lossRatio)) : <Skeleton className="h-6 w-12" />} detail="Of packets received" />
            <Tile label="Recording" value={totals ? formatMinutes(totals.recording_minutes) : <Skeleton className="h-6 w-12" />} detail="Room time" />
            <Tile
              label="Failed connections"
              value={aborted.data ? `${failuresCapped ? "At least " : ""}${failures.length}` : <Skeleton className="h-6 w-12" />}
              testId="analytics-failures"
              detail="From the event log"
            />
          </TileGrid>

          <div className="grid gap-px overflow-hidden rounded-xs border bg-line lg:grid-cols-2">
            <Chart
              title="Concurrent participants"
              description="The most people connected, and rooms open, at once in each period."
              kind="bar"
              data={rows}
              series={[
                { key: "peak_participants", label: "Participants", slot: 1 },
                { key: "peak_rooms", label: "Rooms", slot: 2 },
              ]}
              integer
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatCount}
              empty={emptyHint}
              testId="chart-concurrent"
            />
            <Chart
              title="Streaming minutes"
              description="Time people spent connected, and time rooms were open."
              kind="bar"
              data={rows}
              series={[
                { key: "participant_minutes", label: "Participant", slot: 1 },
                { key: "room_minutes", label: "Room", slot: 2 },
              ]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatMinutesTick}
              formatValue={formatMinutes}
              empty={emptyHint}
              testId="chart-streaming"
            />
            <Chart
              title="Room duration"
              description={history.data?.truncated ? "Average per period, from the newest 1,000 rooms in room history." : "Average for rooms that finished, by the period they started in."}
              kind="bar"
              data={rows}
              series={[{ key: "avg_duration", label: "Average duration", slot: 1 }]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatDurationTick}
              formatValue={(v) => (v < 1 ? `${formatDecimal(v, 2)}s` : formatDuration(v))}
              empty="No room finished in this range."
              testId="chart-duration"
            />
            <Chart
              title="Data transferred"
              kind="stacked"
              data={rows}
              series={[
                { key: "bytes_in", label: "In", slot: 1 },
                { key: "bytes_out", label: "Out", slot: 2 },
              ]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatBytesTick}
              formatValue={formatBytes}
              empty={emptyHint}
              testId="chart-data"
            />
            <Chart
              title="Audio, video and recording"
              description="Track minutes published, and time rooms were recorded."
              kind="bar"
              data={rows}
              series={[
                { key: "audio_minutes", label: "Audio", slot: 1 },
                { key: "video_minutes", label: "Video", slot: 2 },
                { key: "recording_minutes", label: "Recording", slot: 3 },
              ]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatMinutesTick}
              formatValue={formatMinutes}
              empty={emptyHint}
            />
            <Chart
              title="Round trip time"
              description="Average on subscribed media in each period."
              data={rows}
              series={[{ key: "rtt", label: "RTT", slot: 1 }]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatMsTick}
              formatValue={(v) => `${formatDecimal(v)} ms`}
              empty="Not measured in this range. RTT needs someone subscribed to a track."
            />
            <Chart
              title="Packet loss"
              description="Share of incoming packets lost in each period."
              data={rows}
              series={[{ key: "loss", label: "Loss", slot: 1 }]}
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={(v) => `${formatDecimal(v, 2)}%`}
              yDomain={[0, (max: number) => Math.max(1, Math.ceil(max))]}
              empty={emptyHint}
            />
            <Chart
              title="Failed connections"
              description="People who signalled, but could not establish media."
              kind="bar"
              data={rows}
              series={[{ key: "failures", label: "Aborted connections", slot: 1 }]}
              integer
              formatX={fmtX}
              formatXLong={fmtLong}
              formatY={formatCount}
              empty="None in the event log for this range."
            />
          </div>

          <p className="text-xs text-ink-3">
            Failed connections are counted from{" "}
            <Link className="link" href="/logs?q=participant_connection_aborted">
              the event log
            </Link>
            , which keeps 30 days, so older ranges read low. Other figures come from usage records, kept for 400 days.
          </p>
        </div>
      )}
    </Page>
  );
}
