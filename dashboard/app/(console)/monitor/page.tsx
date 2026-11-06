"use client";

import { ChevronDown, ChevronRight } from "lucide-react";
import Link from "next/link";
import * as React from "react";
import { Chart } from "@/components/charts/chart";
import { EmptyState, Page, Panel } from "@/components/console/page";
import { participantHref, RemoveParticipant } from "@/components/console/participants";
import { QualityBadge, RoomStateBadge } from "@/components/console/status";
import { Meter, NotMeasured, Tile, TileGrid } from "@/components/console/tile";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton, TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { formatBitrate, formatBitrateTick, formatBytes, formatClock, formatCount, formatDecimal, formatDuration, formatMsTick } from "@/lib/format";
import { HISTORY_SECONDS, rollingDomain, useLive } from "@/lib/live";
import type { ParticipantStat, ProjectSnapshot } from "@/lib/types";

const clockX = (x: number | string) => formatClock(new Date(x));
const ms = (v: number) => `${formatDecimal(v)} ms`;
const percent = (v: number) => `${formatDecimal(v, 1)}%`;
const rate = (v: number) => `${formatCount(Math.round(v))}/s`;
const dash = <NotMeasured>–</NotMeasured>;

function ParticipantRows({ p }: { p: ParticipantStat }) {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Tr data-testid="monitor-participant" data-identity={p.identity} data-bitrate-in={p.bitrate_in}>
        <Td className="max-w-48">
          <button type="button" className="flex min-w-0 items-center gap-1 text-left" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {open ? <ChevronDown className="size-3.5 shrink-0 text-ink-3" aria-hidden /> : <ChevronRight className="size-3.5 shrink-0 text-ink-3" aria-hidden />}
            <span className="truncate font-medium">{p.identity}</span>
          </button>
        </Td>
        <Td className="hidden max-w-32 truncate text-ink-2 md:table-cell">{p.room}</Td>
        <Td>
          <QualityBadge quality={p.quality} />
        </Td>
        <Td className="num text-right whitespace-nowrap" data-testid="monitor-bitrate-in">
          {formatBitrate(p.bitrate_in)}
        </Td>
        <Td className="num text-right whitespace-nowrap">{formatBitrate(p.bitrate_out)}</Td>
        <Td className="num hidden text-right lg:table-cell">{p.packets_in.toLocaleString("en")}</Td>
        <Td className="num hidden text-right lg:table-cell">{p.packets_out.toLocaleString("en")}</Td>
        <Td className="num hidden text-right sm:table-cell">{`${(p.loss_ratio * 100).toFixed(1)}%`}</Td>
        <Td className="num hidden text-right sm:table-cell">{p.rtt_ms > 0 ? ms(p.rtt_ms) : dash}</Td>
        <Td className="num hidden text-right md:table-cell">{p.jitter_ms > 0 ? ms(p.jitter_ms) : dash}</Td>
        <Td className="w-8 px-1 text-right">
          <RemoveParticipant participant={p} />
        </Td>
      </Tr>
      {open && (
        <tr>
          <td colSpan={11} className="border-b bg-sunken px-3 py-2">
            {p.tracks.length === 0 ? (
              <p className="text-sm text-ink-3">Not publishing anything.</p>
            ) : (
              <ul className="flex flex-col gap-1.5" aria-label={`Tracks of ${p.identity}`}>
                {p.tracks.map((t) => (
                  <li key={t.sid} data-testid="monitor-track" data-kind={t.kind} className="flex flex-wrap items-baseline gap-x-4 gap-y-0.5 text-xs">
                    <span className="w-28 font-medium capitalize">{t.source.toLowerCase().replace(/_/g, " ")}</span>
                    <span className="text-ink-2">{t.mime_type.split("/")[1]}</span>
                    <span className="num">{formatBitrate(t.bitrate_in)}</span>
                    {t.kind === "video" && (
                      <>
                        <span className="num" data-testid="monitor-resolution">
                          {t.width && t.height ? `${t.width}×${t.height}` : "Size unknown"}
                        </span>
                        <span className="num" data-testid="monitor-fps">
                          {t.fps} fps
                        </span>
                        <span className="num">{t.layers} layers</span>
                      </>
                    )}
                    <span className="num">{t.packets_in.toLocaleString("en")} packets</span>
                    <span className="num">{t.packets_lost.toLocaleString("en")} lost</span>
                    <span className="num">
                      {t.subscribers} {t.subscribers === 1 ? "subscriber" : "subscribers"}
                    </span>
                    {t.muted && <span className="text-warn-text">Muted</span>}
                  </li>
                ))}
              </ul>
            )}
            <div className="mt-2">
              <Link className="link text-xs" href={participantHref(p.room, p.identity)}>
                Inspect {p.identity}
              </Link>
            </div>
          </td>
        </tr>
      )}
    </>
  );
}

function Totals({ s }: { s: ProjectSnapshot }) {
  const mem = s.node.memory_total ? (s.node.memory_used / s.node.memory_total) * 100 : 0;
  return (
    <TileGrid className="grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 2xl:grid-cols-12">
      <Tile label="Rooms" value={s.active_rooms} testId="monitor-rooms" />
      <Tile label="Participants" value={s.participants} testId="monitor-participants" />
      <Tile label="Audio streams" value={s.audio_streams} testId="monitor-audio" />
      <Tile label="Video streams" value={s.video_streams} testId="monitor-video" />
      <Tile label="Bitrate in" value={formatBitrate(s.bitrate_in)} testId="monitor-total-in" />
      <Tile label="Bitrate out" value={formatBitrate(s.bitrate_out)} />
      <Tile label="Packets in" value={rate(s.packets_in_rate)} testId="monitor-packets-in" />
      <Tile label="Packets out" value={rate(s.packets_out_rate)} />
      <Tile label="Loss" value={`${(s.packet_loss_ratio * 100).toFixed(1)}%`} />
      <Tile label="RTT" value={s.avg_rtt_ms > 0 ? ms(s.avg_rtt_ms) : <NotMeasured />} />
      <Tile label="Server CPU" value={`${s.node.cpu_percent.toFixed(0)}%`} detail={`This process ${s.node.process_cpu_percent.toFixed(0)}%`}>
        <Meter percent={s.node.cpu_percent} label="Server CPU" />
      </Tile>
      <Tile label="Server memory" value={`${mem.toFixed(0)}%`} detail={`This process ${formatBytes(s.node.process_memory)}`}>
        <Meter percent={mem} label="Server memory" />
      </Tile>
    </TileGrid>
  );
}

export default function MonitorPage() {
  const snapshot = useLive((s) => s.snapshot);
  const history = useLive((s) => s.history);
  const status = useLive((s) => s.status);
  const domain = rollingDomain(history);
  const memTotal = snapshot?.node.memory_total ?? 0;

  const rows = React.useMemo(
    () =>
      history.map((h) => ({
        x: h.t,
        bitrate_in: h.bitrate_in,
        bitrate_out: h.bitrate_out,
        packets_in: h.packets_in_rate,
        packets_out: h.packets_out_rate,
        rtt: h.rtt_ms,
        jitter: h.jitter_ms,
        loss: h.loss_percent,
        cpu: h.cpu_percent,
        process_cpu: h.process_cpu_percent,
        memory: h.memory_percent,
        participants: h.participants,
      })),
    [history],
  );

  return (
    <Page
      title="Live monitor"
      description={`Measured on real media, one reading a second. The charts show the last ${HISTORY_SECONDS / 60} minutes from when this tab connected; they live in your browser and are gone when you reload.`}
    >
      {status === "polling" && (
        <Callout tone="warn" title="The live WebSocket is not connected">
          Figures are fetched over HTTP every 3 seconds instead. Check that the page&rsquo;s address matches the server&rsquo;s <code className="mono">public_dashboard_url</code>, and that <code className="mono">NEXT_PUBLIC_STREAMKIT_WS_ORIGIN</code> points at the server.
        </Callout>
      )}
      {status === "offline" && (
        <Callout tone="critical" title="The server is not answering">
          The last reading is on screen. It will update when the server is back.
        </Callout>
      )}
      {!snapshot ? (
        <Skeleton className="h-20" />
      ) : (
        <Totals s={snapshot} />
      )}
      <div className="grid gap-px overflow-hidden rounded-xs border bg-line md:grid-cols-2 2xl:grid-cols-3">
        <Chart
          title="Bitrate"
          data={rows}
          series={[
            { key: "bitrate_in", label: "In", slot: 1 },
            { key: "bitrate_out", label: "Out", slot: 2 },
          ]}
          latest={snapshot ? { bitrate_in: formatBitrate(snapshot.bitrate_in), bitrate_out: formatBitrate(snapshot.bitrate_out) } : undefined}
          domain={domain}
          formatX={clockX}
          formatY={formatBitrateTick}
          formatValue={formatBitrate}
          testId="chart-bitrate"
          empty="Waiting for the first reading."
        />
        <Chart
          title="Packets"
          data={rows}
          series={[
            { key: "packets_in", label: "In", slot: 1 },
            { key: "packets_out", label: "Out", slot: 2 },
          ]}
          latest={snapshot ? { packets_in: rate(snapshot.packets_in_rate), packets_out: rate(snapshot.packets_out_rate) } : undefined}
          domain={domain}
          formatX={clockX}
          formatY={rate}
          empty="Waiting for the first reading."
        />
        <Chart
          title="RTT and jitter"
          description="Measured on media people receive."
          data={rows}
          series={[
            { key: "rtt", label: "RTT", slot: 1 },
            { key: "jitter", label: "Jitter", slot: 2 },
          ]}
          latest={snapshot ? { rtt: snapshot.avg_rtt_ms > 0 ? ms(snapshot.avg_rtt_ms) : "–", jitter: snapshot.avg_jitter_ms > 0 ? ms(snapshot.avg_jitter_ms) : "–" } : undefined}
          domain={domain}
          formatX={clockX}
          formatY={formatMsTick}
          formatValue={ms}
          empty="Not measured until someone subscribes to a track."
        />
        <Chart
          title="Packet loss"
          description="The worst participant at each reading."
          data={rows}
          series={[{ key: "loss", label: "Loss", slot: 1 }]}
          domain={domain}
          formatX={clockX}
          formatY={percent}
          yDomain={[0, (max: number) => Math.max(1, Math.ceil(max))]}
          empty="Waiting for the first reading."
        />
        <Chart
          title="Server CPU"
          data={rows}
          series={[
            { key: "cpu", label: "Machine", slot: 1 },
            { key: "process_cpu", label: "This process", slot: 2 },
          ]}
          latest={snapshot ? { cpu: `${snapshot.node.cpu_percent.toFixed(0)}%`, process_cpu: `${snapshot.node.process_cpu_percent.toFixed(0)}%` } : undefined}
          domain={domain}
          formatX={clockX}
          formatY={(v) => `${v.toFixed(0)}%`}
          formatValue={percent}
          empty="Waiting for the first reading."
        />
        <Chart
          title="Server memory"
          description={memTotal ? `Of ${formatBytes(memTotal)}` : undefined}
          data={rows}
          series={[{ key: "memory", label: "Used", slot: 1 }]}
          domain={domain}
          formatX={clockX}
          formatY={(v) => `${v.toFixed(0)}%`}
          formatValue={percent}
          yDomain={[0, 100]}
          empty="Waiting for the first reading."
        />
      </div>
      <Panel title="Rooms">
        {!snapshot ? (
          <TableSkeleton rows={2} />
        ) : snapshot.rooms.length === 0 ? (
          <EmptyState title="No rooms are open" className="py-6" action={<Button asChild><Link href="/rooms?new=1">Create a room</Link></Button>} />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Room</Th>
                <Th>Status</Th>
                <Th className="text-right">People</Th>
                <Th className="hidden text-right sm:table-cell">Audio</Th>
                <Th className="hidden text-right sm:table-cell">Video</Th>
                <Th className="text-right">In</Th>
                <Th className="text-right">Out</Th>
                <Th className="hidden text-right md:table-cell">Open for</Th>
              </tr>
            </thead>
            <tbody>
              {snapshot.rooms.map((r) => (
                <Tr key={r.sid}>
                  <Td className="max-w-48 truncate">
                    <Link className="link" href={`/rooms/${encodeURIComponent(r.name)}`}>
                      {r.name}
                    </Link>
                  </Td>
                  <Td>
                    <RoomStateBadge state={r.state} />
                  </Td>
                  <Td className="num text-right">{r.participants}</Td>
                  <Td className="num hidden text-right sm:table-cell">{r.audio_streams}</Td>
                  <Td className="num hidden text-right sm:table-cell">{r.video_streams}</Td>
                  <Td className="num text-right whitespace-nowrap">{formatBitrate(r.bitrate_in)}</Td>
                  <Td className="num text-right whitespace-nowrap">{formatBitrate(r.bitrate_out)}</Td>
                  <Td className="num hidden text-right whitespace-nowrap md:table-cell">{formatDuration(r.duration_seconds)}</Td>
                </Tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <Panel title="Participants and their streams" description="Expand someone to see each track's bitrate, resolution and frame rate.">
        {!snapshot ? (
          <TableSkeleton rows={2} />
        ) : snapshot.participant_stats.length === 0 ? (
          <EmptyState title="Nobody is connected" className="py-6">
            <p>Connect a client and it shows here within a second.</p>
          </EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Identity</Th>
                <Th className="hidden md:table-cell">Room</Th>
                <Th>Network</Th>
                <Th className="text-right">In</Th>
                <Th className="text-right">Out</Th>
                <Th className="hidden text-right lg:table-cell">Packets in</Th>
                <Th className="hidden text-right lg:table-cell">Packets out</Th>
                <Th className="hidden text-right sm:table-cell">Loss</Th>
                <Th className="hidden text-right sm:table-cell">RTT</Th>
                <Th className="hidden text-right md:table-cell">Jitter</Th>
                <Th className="w-8">
                  <span className="sr-only">Remove</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {snapshot.participant_stats.map((p) => (
                <ParticipantRows key={p.sid} p={p} />
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
    </Page>
  );
}
