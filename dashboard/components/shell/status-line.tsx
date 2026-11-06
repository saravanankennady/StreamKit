"use client";

import { useLive, type LiveStatus } from "@/lib/live";
import { useNow } from "@/lib/hooks";
import { formatBitrate, formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";

const labels: Record<LiveStatus, { text: string; dot: string; title: string }> = {
  idle: { text: "Not connected", dot: "bg-ink-3", title: "" },
  connecting: { text: "Connecting", dot: "bg-ink-3", title: "Opening the live feed." },
  live: { text: "Live", dot: "bg-good pulse-dot", title: "Receiving a snapshot every second over the WebSocket." },
  polling: {
    text: "Polling",
    dot: "bg-warn",
    title: "The live WebSocket is not connected, so figures are fetched every 3 seconds over HTTP. Check NEXT_PUBLIC_STREAMKIT_WS_ORIGIN, and that public_dashboard_url matches this page's address.",
  },
  offline: { text: "No data", dot: "bg-critical", title: "Neither the WebSocket nor the HTTP API is answering. Figures on screen may be out of date." },
};

function Reading({ label, value, className, testId }: { label: string; value: string; className?: string; testId?: string }) {
  return (
    <span className={cn("shrink-0 whitespace-nowrap", className)}>
      <span className="text-ink-3">{label} </span>
      <span className="num text-ink" data-testid={testId}>
        {value}
      </span>
    </span>
  );
}

/**
 * The console's signature: a status line that is on every page, like the one in an editor or tmux. At 3am you can
 * see whether the node is healthy and how much it is carrying without going anywhere.
 */
export function StatusLine() {
  const { status, snapshot, lastSampleAt } = useLive();
  const now = useNow(1000);
  const age = lastSampleAt ? Math.floor((now - lastSampleAt) / 1000) : null;
  const stale = age !== null && age > 5;
  const label = labels[status];
  const node = snapshot?.node;
  return (
    <footer
      data-testid="status-line"
      className="mono scroll-quiet sticky bottom-0 z-30 flex h-7 items-center gap-4 overflow-x-auto border-t bg-surface px-3 text-[11px] leading-none whitespace-nowrap"
    >
      <span className="flex shrink-0 items-center gap-1.5" title={label.title} data-testid="live-status" data-status={status}>
        <span aria-hidden className={cn("size-1.5 rounded-full", label.dot)} />
        <span className="text-ink">{label.text}</span>
        {stale && <span className="text-warn-text">{age}s old</span>}
      </span>
      {node && (
        <>
          <span className="flex shrink-0 items-center gap-1.5">
            <span className="hidden text-ink-3 sm:inline">{node.node_id}</span>
            {node.draining ? <span className="text-warn-text">Draining</span> : node.healthy ? <span className="text-good-text">Healthy</span> : <span className="text-critical-text">Unhealthy</span>}
          </span>
          <Reading label="CPU" value={`${node.cpu_percent.toFixed(0)}%`} />
          <Reading label="Mem" value={node.memory_total ? `${((node.memory_used / node.memory_total) * 100).toFixed(0)}%` : formatBytes(node.memory_used)} />
        </>
      )}
      {snapshot && (
        <>
          <Reading label="Rooms" value={String(snapshot.active_rooms)} testId="statusline-rooms" />
          <Reading label="People" value={String(snapshot.participants)} testId="statusline-participants" />
          <Reading label="In" value={formatBitrate(snapshot.bitrate_in)} className="hidden sm:inline" />
          <Reading label="Out" value={formatBitrate(snapshot.bitrate_out)} className="hidden sm:inline" />
          <Reading label="RTT" value={snapshot.avg_rtt_ms > 0 ? `${snapshot.avg_rtt_ms.toFixed(0)} ms` : "–"} className="hidden md:inline" />
          <Reading label="Loss" value={`${(snapshot.packet_loss_ratio * 100).toFixed(1)}%`} className="hidden md:inline" />
        </>
      )}
    </footer>
  );
}
