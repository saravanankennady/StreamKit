"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clapperboard, Download, Play, RotateCcw, Square, Trash2 } from "lucide-react";
import * as React from "react";
import { EgressStatusBadge, StartEgressDialog, egressTypeLabel, isRunning, useJobList } from "@/components/console/egress";
import { EgressWorkerNotice } from "@/components/console/workers";
import { EmptyState, ErrorState, Page, Panel, ServiceNotRunning } from "@/components/console/page";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { ApiError, isRouteMissing } from "@/lib/api";
import { formatBytes, formatDateTime, formatDuration, formatShortTime } from "@/lib/format";
import { useProject, useProjectApi } from "@/lib/session";
import type { EgressJob } from "@/lib/types";

/** A video element cannot send headers, so it names the project in the query string, which the API also accepts. */
function fileUrl(job: EgressJob, projectId: string) {
  const sep = job.output_url.includes("?") ? "&" : "?";
  return `${job.output_url}${sep}project_id=${encodeURIComponent(projectId)}`;
}

function RecordingRow({ job, onPlay, onDelete }: { job: EgressJob; onPlay: () => void; onDelete: () => void }) {
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const retry = useMutation<EgressJob, ApiError>({
    mutationFn: () => call<EgressJob>(`/recordings/${encodeURIComponent(job.id)}/retry`, { method: "POST" }),
    onSuccess: (next) => {
      toast("Started again", `New job ${next.id}.`);
      return qc.invalidateQueries({ queryKey: [projectId, "media"] });
    },
    onError: (err) => toastError(err, "Could not retry"),
  });
  const stop = useMutation<EgressJob, ApiError>({
    mutationFn: () => call<EgressJob>(`/egress/${encodeURIComponent(job.id)}/stop`, { method: "POST" }),
    onSuccess: () => {
      toast("Stopping the recording", "The file appears here when it has been saved.");
      return qc.invalidateQueries({ queryKey: [projectId, "media"] });
    },
    onError: (err) => toastError(err, "Could not stop the recording"),
  });
  const done = job.status === "complete" && !!job.output_url;
  const canRetry = job.status === "failed" || job.status === "aborted";
  return (
    <Tr data-testid="recording-row" data-id={job.id} data-status={job.status}>
      <Td className="mono hidden max-w-40 truncate text-xs md:table-cell" title={job.id}>
        {job.id}
      </Td>
      <Td className="max-w-32 truncate sm:max-w-40">{job.room_name}</Td>
      <Td className="hidden whitespace-nowrap text-ink-2 lg:table-cell">{egressTypeLabel(job.type)}</Td>
      <Td>
        <EgressStatusBadge status={job.status} />
        {job.error && (
          <p className="mt-0.5 max-w-64 truncate text-xs text-critical-text" title={job.error}>
            {job.error}
          </p>
        )}
      </Td>
      <Td className="num hidden text-right whitespace-nowrap sm:table-cell">{job.duration_ms > 0 ? formatDuration(job.duration_ms / 1000) : "–"}</Td>
      <Td className="num hidden text-right whitespace-nowrap sm:table-cell">{job.file_size > 0 ? formatBytes(job.file_size) : "–"}</Td>
      <Td className="hidden text-ink-2 uppercase md:table-cell">{job.format}</Td>
      <Td className="hidden whitespace-nowrap text-ink-2 md:table-cell" title={formatDateTime(job.created_at)}>
        {formatShortTime(job.created_at)}
      </Td>
      <Td className="text-right whitespace-nowrap">
        <span className="inline-flex gap-1">
          {done && (
            <>
              <Button size="sm" data-testid="recording-play" aria-label={`Play ${job.id}`} onClick={onPlay}>
                <Play aria-hidden /> <span className="hidden sm:inline">Play</span>
              </Button>
              <Button asChild size="sm" variant="ghost">
                <a href={`${fileUrl(job, projectId)}&download=1`} download aria-label={`Download ${job.id}`} data-testid="recording-download">
                  <Download aria-hidden /> <span className="hidden sm:inline">Download</span>
                </a>
              </Button>
            </>
          )}
          {isRunning(job.status) && job.status !== "ending" && (
            <Button size="sm" variant="danger" pending={stop.isPending} data-testid="recording-stop" onClick={() => stop.mutate()}>
              <Square aria-hidden /> Stop
            </Button>
          )}
          {canRetry && (
            <Button size="sm" pending={retry.isPending} onClick={() => retry.mutate()}>
              <RotateCcw aria-hidden /> Retry
            </Button>
          )}
          {!isRunning(job.status) && (
            <Button size="iconSm" variant="ghost" aria-label={`Delete ${job.id}`} onClick={onDelete}>
              <Trash2 aria-hidden />
            </Button>
          )}
        </span>
      </Td>
    </Tr>
  );
}

export default function RecordingsPage() {
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const list = useJobList("recordings", "/recordings");
  const [starting, setStarting] = React.useState(false);
  const [playing, setPlaying] = React.useState<EgressJob | null>(null);
  const [deleting, setDeleting] = React.useState<EgressJob | null>(null);
  const missing = isRouteMissing(list.error);
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Page
      title="Recordings"
      description="Room captures saved to a file on this server. Play them here, download them, or start one."
      actions={
        !missing && (
          <Button variant="primary" onClick={() => setStarting(true)}>
            <Clapperboard aria-hidden /> Start recording
          </Button>
        )
      }
    >
      {!missing && !list.isPending && <EgressWorkerNotice />}
      <Panel>
        {list.isPending ? (
          <TableSkeleton rows={3} />
        ) : missing ? (
          <ServiceNotRunning name="recording" />
        ) : list.error ? (
          <ErrorState error={list.error} onRetry={() => list.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState icon={Clapperboard} title="No recordings yet" action={<Button variant="primary" onClick={() => setStarting(true)}>Start recording</Button>}>
            <p>Start one on an open room. The file appears here when it finishes, ready to play and download.</p>
          </EmptyState>
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th className="hidden md:table-cell">ID</Th>
                  <Th>Room</Th>
                  <Th className="hidden lg:table-cell">Type</Th>
                  <Th>Status</Th>
                  <Th className="hidden text-right sm:table-cell">Length</Th>
                  <Th className="hidden text-right sm:table-cell">Size</Th>
                  <Th className="hidden md:table-cell">Format</Th>
                  <Th className="hidden md:table-cell">Created</Th>
                  <Th>
                    <span className="sr-only">Actions</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {items.map((job) => (
                  <RecordingRow key={job.id} job={job} onPlay={() => setPlaying(job)} onDelete={() => setDeleting(job)} />
                ))}
              </tbody>
            </Table>
            {list.hasNextPage && (
              <div className="p-3">
                <Button size="sm" pending={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>
                  Load older recordings
                </Button>
              </div>
            )}
          </>
        )}
      </Panel>
      {items.some((j) => j.status === "failed") && <Callout tone="serious" title="Some recordings failed">Each row says why. Retry starts a new job with the same request, and the room must still be open.</Callout>}
      <StartEgressDialog open={starting} onOpenChange={setStarting} recording />
      <Dialog open={!!playing} onOpenChange={(o) => !o && setPlaying(null)}>
        <DialogContent title={playing ? `${playing.room_name}, ${formatShortTime(playing.created_at)}` : ""} className="max-w-3xl">
          {playing && (
            <div className="flex flex-col gap-3 p-4">
              {/* The file route supports Range, so seeking works. */}
              <video data-testid="recording-video" controls autoPlay className="aspect-video w-full rounded-xs bg-black" src={fileUrl(playing, projectId)}>
                <track kind="captions" />
              </video>
              <div className="flex flex-wrap items-center justify-between gap-2 text-sm text-ink-2">
                <span>
                  {playing.duration_ms > 0 ? formatDuration(playing.duration_ms / 1000) : "Length unknown"}, {playing.file_size > 0 ? formatBytes(playing.file_size) : "size unknown"}
                </span>
                <Button asChild size="sm">
                  <a href={`${fileUrl(playing, projectId)}&download=1`} download>
                    <Download aria-hidden /> Download
                  </a>
                </Button>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={`Delete ${deleting?.id}?`}
        description="The file and its record are both deleted. This cannot be undone."
        confirmLabel="Delete recording"
        onConfirm={async () => {
          await call(`/recordings/${encodeURIComponent(deleting?.id ?? "")}`, { method: "DELETE" });
          await qc.invalidateQueries({ queryKey: [projectId, "media"] });
          toast("Recording deleted");
        }}
      />
    </Page>
  );
}
