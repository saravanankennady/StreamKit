"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Server } from "lucide-react";
import * as React from "react";
import { EmptyState, ErrorState, Page, Panel } from "@/components/console/page";
import { Meter } from "@/components/console/tile";
import { Callout, StatusBadge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyValue } from "@/components/ui/copy";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "@/components/ui/toast";
import { api, ApiError } from "@/lib/api";
import { formatBytes, formatDateTime, formatDuration, formatRelative } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { useProject } from "@/lib/session";
import type { Items, ServerNode } from "@/lib/types";

/** Heartbeats come every 10 seconds. Missing a couple means something is wrong with the node or its database link. */
const STALE_MS = 30_000;

const KINDS = {
  egress_worker: { label: "Recording worker", work: "jobs" },
  ingress_worker: { label: "Ingest worker", work: "streams" },
} as const;

function availability(n: ServerNode, now: number): { tone: Tone; label: string; detail: string } {
  const worker = n.stats.kind ? KINDS[n.stats.kind] : null;
  if (now - Date.parse(n.last_seen_at) > STALE_MS) return { tone: "critical", label: "Not reporting", detail: "It has missed its heartbeats. Nodes that stay silent for a minute drop off this list." };
  if (!n.stats.healthy) return { tone: "critical", label: "Unhealthy", detail: worker ? `It takes no new ${worker.work} until this is fixed.` : "It reports itself as unhealthy." };
  if (n.draining || n.stats.draining) return { tone: "warn", label: "Draining", detail: worker ? `It finishes the ${worker.work} it has, and takes no new ones.` : "It finishes the rooms it has, and takes no new ones." };
  if (worker) return { tone: "good", label: `Accepting ${worker.work}`, detail: `${worker.label}, reporting every 5 seconds.` };
  return { tone: "good", label: "Accepting sessions", detail: "Media server, reporting every 10 seconds, and taking new rooms." };
}

function Reading({ label, value, detail, children }: { label: string; value: React.ReactNode; detail?: React.ReactNode; children?: React.ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1 bg-surface px-3 py-2.5">
      <div className="text-xs text-ink-3">{label}</div>
      <div className="text-lg leading-6 font-semibold tracking-tight">{value}</div>
      {children}
      {detail && <div className="text-xs text-ink-3">{detail}</div>}
    </div>
  );
}

function NodeCard({ node, isAdmin, now }: { node: ServerNode; isAdmin: boolean; now: number }) {
  const qc = useQueryClient();
  const [confirm, setConfirm] = React.useState(false);
  const s = node.stats;
  const state = availability(node, now);
  const kind = s.kind ? KINDS[s.kind] : null;
  const draining = node.draining || s.draining;
  const memPercent = s.memory_total ? (s.memory_used / s.memory_total) * 100 : 0;
  return (
    <Panel
      className="flex flex-col"
      title={
        <span className="flex flex-wrap items-center gap-2">
          <CopyValue value={node.node_id} className="text-sm" /> <StatusBadge tone={state.tone}>{state.label}</StatusBadge>
        </span>
      }
      description={state.detail}
      actions={
        isAdmin &&
        (draining ? (
          <Button size="sm" data-testid="node-resume" onClick={() => setConfirm(true)}>
            Resume
          </Button>
        ) : (
          <Button size="sm" variant="danger" data-testid="node-drain" onClick={() => setConfirm(true)}>
            Drain
          </Button>
        ))
      }
    >
      {!s.healthy && s.unhealthy_reason && (
        <Callout tone="serious" title="What it says is wrong" className="m-3">
          <span data-testid="unhealthy-reason">{s.unhealthy_reason}</span>
        </Callout>
      )}
      <div data-testid="server-card" data-kind={s.kind ?? "server"} data-draining={draining} className={isAdmin ? `grid grid-cols-2 gap-px bg-line ${kind ? "sm:grid-cols-3 max-sm:[&>*:last-child]:col-span-2" : "sm:grid-cols-4"}` : undefined}>
        {isAdmin && (
          <>
            <Reading label="CPU" value={`${s.cpu_percent.toFixed(0)}%`} detail={s.process_cpu_percent === undefined ? `${s.num_cpu} cores` : `${s.num_cpu} cores, this process ${s.process_cpu_percent.toFixed(0)}%`}>
              <Meter percent={s.cpu_percent} label="CPU" />
            </Reading>
            <Reading label="Memory" value={`${memPercent.toFixed(0)}%`} detail={s.process_memory === undefined ? `${formatBytes(s.memory_used)} of ${formatBytes(s.memory_total)}` : `${formatBytes(s.memory_used)} of ${formatBytes(s.memory_total)}, this process ${formatBytes(s.process_memory)}`}>
              <Meter percent={memPercent} label="Memory" />
            </Reading>
            {kind ? (
              <Reading label={kind === KINDS.egress_worker ? "Recordings running" : "Streams running"} value={s.active_jobs ?? 0} detail={s.max_jobs ? `Room for ${s.max_jobs} at once` : undefined} />
            ) : (
              <>
                <Reading label="Active rooms" value={s.active_rooms ?? 0} />
                <Reading label="Participants" value={s.participants ?? 0} />
              </>
            )}
          </>
        )}
      </div>
      <dl className="grid grid-cols-[repeat(auto-fill,minmax(8.5rem,1fr))] gap-x-4 gap-y-2 border-t p-3 text-sm">
        <div>
          <dt className="text-xs text-ink-3">Version</dt>
          <dd className="mono" data-testid="node-version">
            {node.version}
          </dd>
        </div>
        <div>
          <dt className="text-xs text-ink-3">Uptime</dt>
          <dd title={`Started ${formatDateTime(node.started_at)}`}>{formatDuration(s.uptime_seconds)}</dd>
        </div>
        <div>
          <dt className="text-xs text-ink-3">Last heartbeat</dt>
          <dd title={formatDateTime(node.last_seen_at)}>{formatRelative(node.last_seen_at, now)}</dd>
        </div>
        <div>
          <dt className="text-xs text-ink-3">Region</dt>
          <dd>{node.region || <span className="text-ink-3">Not set</span>}</dd>
        </div>
        {isAdmin && (
          <>
            <div>
              <dt className="text-xs text-ink-3">Host</dt>
              <dd className="truncate">{s.hostname}</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-3">Address</dt>
              <dd className="mono truncate text-xs" title={node.address}>
                {node.address || <span className="text-ink-3">Not set</span>}
              </dd>
            </div>
          </>
        )}
        {isAdmin && s.goroutines !== undefined && (
          <div>
            <dt className="text-xs text-ink-3">Goroutines</dt>
            <dd className="num">{s.goroutines.toLocaleString("en")}</dd>
          </div>
        )}
      </dl>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        destructive={!draining}
        title={draining ? `Resume ${node.node_id}?` : `Drain ${node.node_id}?`}
        description={
          draining
            ? `It starts taking new ${kind ? kind.work : "rooms"} again.`
            : kind
              ? `It finishes the ${kind.work} it has, and takes no new ones. With no other worker, nothing new can start until you resume it.`
              : `It stops taking new rooms and new sessions. The ${s.active_rooms ?? 0} ${s.active_rooms === 1 ? "room" : "rooms"} it has keep running. On a single-node install, nobody can start a new room until you resume it.`
        }
        confirmLabel={draining ? "Resume node" : "Drain node"}
        onConfirm={async () => {
          await api(`/servers/${encodeURIComponent(node.node_id)}/drain`, { method: "POST", body: { draining: !draining } });
          await qc.invalidateQueries({ queryKey: ["servers"] });
          toast(draining ? `${node.node_id} resumed` : `${node.node_id} is draining`);
        }}
      />
    </Panel>
  );
}

export default function ServersPage() {
  const { me } = useProject();
  const now = useNow(1000);
  const servers = useQuery<Items<ServerNode>, ApiError>({ queryKey: ["servers"], queryFn: ({ signal }) => api("/servers", { signal }), refetchInterval: 5000 });
  const isAdmin = me.user.is_platform_admin;
  return (
    <Page title="Servers" description="Media servers and media workers on this installation that reported in the last minute. Figures come from each node's heartbeat.">
      {!isAdmin && <Callout title="Only platform administrators can see machine details and drain a node">This installation can be shared with other organizations, so its hosts, addresses and load are not shown to everyone. The first account created on it is its administrator.</Callout>}
      {servers.isPending ? (
        <Panel>
          <div className="p-3">
            <Skeleton className="h-36" />
          </div>
        </Panel>
      ) : servers.error ? (
        <Panel>
          <ErrorState error={servers.error} onRetry={() => servers.refetch()} />
        </Panel>
      ) : servers.data.items.length === 0 ? (
        <Panel>
          <EmptyState icon={Server} title="No server has reported in the last minute">
            <p>A running node sends a heartbeat every 10 seconds, so an empty list means the node serving this API cannot write to the database, or has only just started. Check its logs for &ldquo;heartbeat failed&rdquo;.</p>
          </EmptyState>
        </Panel>
      ) : (
        <div className="flex flex-col gap-4">
          {servers.data.items.map((node) => (
            <NodeCard key={node.node_id} node={node} isAdmin={isAdmin} now={now} />
          ))}
        </div>
      )}
      <p className="text-xs text-ink-3">The API reports whether a node is up now. It keeps no availability history, so there is no uptime percentage here.</p>
    </Page>
  );
}
