"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { Callout } from "@/components/ui/badge";
import { api, ApiError } from "@/lib/api";
import type { Items, ServerNode, WorkerKind } from "@/lib/types";

/** Workers report every 5 seconds. The server stops counting one after 15 seconds of silence, and so does this. */
const WORKER_SEEN_WITHIN_MS = 15_000;

export function useWorkers(kind: WorkerKind) {
  const servers = useQuery<Items<ServerNode>, ApiError>({ queryKey: ["servers"], queryFn: ({ signal }) => api("/servers", { signal }), refetchInterval: 10_000 });
  const live = (servers.data?.items ?? []).filter((n) => n.stats.kind === kind && !n.draining && Date.now() - Date.parse(n.last_seen_at) < WORKER_SEEN_WITHIN_MS);
  return {
    known: servers.isSuccess,
    count: live.length,
    capacity: live.reduce((sum, n) => sum + (n.stats.max_jobs ?? 0), 0),
    active: live.reduce((sum, n) => sum + (n.stats.active_jobs ?? 0), 0),
  };
}

export function EgressWorkerNotice() {
  const workers = useWorkers("egress_worker");
  if (!workers.known || workers.count > 0) return null;
  return (
    <Callout tone="serious" title="No recording worker is running" className="mb-0">
      Jobs cannot start until one reports in. Run <code className="mono">streamkit-egress --config</code> with your server&rsquo;s config file. It needs Chrome and ffmpeg on the same machine.{" "}
      <Link className="link" href="/servers">
        See what is running
      </Link>
    </Callout>
  );
}

export function IngressWorkerNotice() {
  const workers = useWorkers("ingress_worker");
  if (!workers.known || workers.count > 0) return null;
  return (
    <Callout tone="warn" title="No ingest worker is running">
      RTMP and URL inputs need one, so they will not connect. Run <code className="mono">streamkit-ingress --config</code> with your server&rsquo;s config file. WHIP goes straight to the server, and works without it.{" "}
      <Link className="link" href="/servers">
        See what is running
      </Link>
    </Callout>
  );
}
