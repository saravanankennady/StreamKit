"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, Send, Square } from "lucide-react";
import * as React from "react";
import { EgressStatusBadge, StartEgressDialog, egressTypeLabel, isRunning, useJobList } from "@/components/console/egress";
import { EmptyState, ErrorState, Facts, Page, Panel, ServiceNotRunning } from "@/components/console/page";
import { EgressWorkerNotice } from "@/components/console/workers";
import { Button } from "@/components/ui/button";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { ApiError, isRouteMissing } from "@/lib/api";
import { formatBytes, formatDateTime, formatDuration, formatShortTime } from "@/lib/format";
import { useProject, useProjectApi } from "@/lib/session";
import type { EgressJob } from "@/lib/types";

function outputs(job: EgressJob) {
  const parts: string[] = [];
  if (job.format) parts.push(`${job.format.toUpperCase()} file`);
  if (job.stream_urls.length) parts.push(`${job.stream_urls.length} ${job.stream_urls.length === 1 ? "stream" : "streams"}`);
  return parts.join(", ") || "None";
}

function JobRow({ job }: { job: EgressJob }) {
  const [open, setOpen] = React.useState(false);
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const stop = useMutation<unknown, ApiError>({
    mutationFn: () => call(`/egress/${encodeURIComponent(job.id)}/stop`, { method: "POST" }),
    onSuccess: () => {
      toast("Stopping the job");
      return qc.invalidateQueries({ queryKey: [projectId, "media"] });
    },
    onError: (err) => toastError(err, "Could not stop the job"),
  });
  return (
    <>
      <Tr data-testid="egress-row" data-id={job.id} data-status={job.status}>
        <Td>
          <button type="button" aria-expanded={open} className="flex items-center gap-1 text-left" onClick={() => setOpen((v) => !v)}>
            {open ? <ChevronDown className="size-3.5 text-ink-3" aria-hidden /> : <ChevronRight className="size-3.5 text-ink-3" aria-hidden />}
            <span className="mono max-w-20 truncate text-xs sm:max-w-36">{job.id}</span>
          </button>
        </Td>
        <Td className="max-w-24 truncate sm:max-w-40">{job.room_name}</Td>
        <Td className="hidden whitespace-nowrap text-ink-2 md:table-cell">{egressTypeLabel(job.type)}</Td>
        <Td>
          <EgressStatusBadge status={job.status} />
        </Td>
        <Td className="hidden whitespace-nowrap text-ink-2 sm:table-cell">{outputs(job)}</Td>
        <Td className="hidden whitespace-nowrap text-ink-2 lg:table-cell" title={formatDateTime(job.created_at)}>
          {formatShortTime(job.created_at)}
        </Td>
        <Td className="text-right">
          {isRunning(job.status) && job.status !== "ending" && (
            <Button size="sm" variant="danger" pending={stop.isPending} data-testid="egress-stop" onClick={() => stop.mutate()}>
              <Square aria-hidden /> Stop
            </Button>
          )}
        </Td>
      </Tr>
      {open && (
        <tr>
          <td colSpan={7} className="border-b bg-sunken p-3">
            {job.error && (
              <p className="mb-2 text-sm text-critical-text">
                <span className="font-medium">Error:</span> {job.error}
              </p>
            )}
            <Facts
              className="p-0"
              items={[
                { label: "Room SID", value: <span className="mono text-xs">{job.room_sid || "–"}</span> },
                { label: "Started", value: job.started_at ? formatDateTime(job.started_at) : "Not started" },
                { label: "Ended", value: job.ended_at ? formatDateTime(job.ended_at) : "Not ended" },
                { label: "Length", value: job.duration_ms > 0 ? formatDuration(job.duration_ms / 1000) : "–" },
                { label: "File size", value: job.file_size > 0 ? formatBytes(job.file_size) : "–" },
                ...(job.retry_of ? [{ label: "Retry of", value: <span className="mono text-xs">{job.retry_of}</span> }] : []),
                ...(job.stream_urls.length ? [{ label: "Streaming to", value: <span className="mono text-xs break-all">{job.stream_urls.join(" ")}</span> }] : []),
              ]}
            />
          </td>
        </tr>
      )}
    </>
  );
}

export default function EgressPage() {
  const list = useJobList("egress", "/egress");
  const [starting, setStarting] = React.useState(false);
  const missing = isRouteMissing(list.error);
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Page
      title="Egress"
      description="Send a room out of the platform: to a file, to RTMP destinations like YouTube and Twitch, or to both at once."
      actions={
        !missing && (
          <Button variant="primary" onClick={() => setStarting(true)}>
            <Send aria-hidden /> Start job
          </Button>
        )
      }
    >
      {!missing && !list.isPending && <EgressWorkerNotice />}
      <Panel>
        {list.isPending ? (
          <TableSkeleton rows={3} />
        ) : missing ? (
          <ServiceNotRunning name="egress" />
        ) : list.error ? (
          <ErrorState error={list.error} onRetry={() => list.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState icon={Send} title="No egress jobs yet" action={<Button variant="primary" onClick={() => setStarting(true)}>Start job</Button>}>
            <p>Start one on an open room. Jobs with a file output also appear under Recordings.</p>
          </EmptyState>
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>ID</Th>
                  <Th>Room</Th>
                  <Th className="hidden md:table-cell">Type</Th>
                  <Th>Status</Th>
                  <Th className="hidden sm:table-cell">Output</Th>
                  <Th className="hidden lg:table-cell">Created</Th>
                  <Th>
                    <span className="sr-only">Stop</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {items.map((job) => (
                  <JobRow key={job.id} job={job} />
                ))}
              </tbody>
            </Table>
            {list.hasNextPage && (
              <div className="p-3">
                <Button size="sm" pending={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>
                  Load older jobs
                </Button>
              </div>
            )}
          </>
        )}
      </Panel>
      <StartEgressDialog open={starting} onOpenChange={setStarting} />
    </Page>
  );
}
