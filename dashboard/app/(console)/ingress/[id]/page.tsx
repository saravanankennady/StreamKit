"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, Square, Trash2 } from "lucide-react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import * as React from "react";
import { useForm } from "react-hook-form";
import { INPUT_LABELS, IngressStatusBadge, StreamKeyReveal } from "@/components/console/ingress";
import { EmptyState, ErrorState, Facts, Page, Panel, ServiceNotRunning } from "@/components/console/page";
import { IngressWorkerNotice } from "@/components/console/workers";
import { Callout, StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyValue } from "@/components/ui/copy";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { ApiError, isRouteMissing } from "@/lib/api";
import { formatBitrate, formatClock, formatDateTime, formatDecimal, formatDuration, formatShortTime } from "@/lib/format";
import { applyApiError } from "@/lib/form";
import { useNow } from "@/lib/hooks";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { Ingress, Items, LogEvent } from "@/lib/types";

function EditPanel({ ingress }: { ingress: Ingress }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const [error, setError] = React.useState<string | null>(null);
  const form = useForm({
    values: { name: ingress.name, room: ingress.room_name, participant_identity: ingress.participant_identity, participant_name: ingress.participant_name },
    resetOptions: { keepDirtyValues: true },
  });
  const submit = form.handleSubmit(async (v) => {
    setError(null);
    try {
      await call(`/ingress/${encodeURIComponent(ingress.id)}`, {
        method: "PATCH",
        body: { name: v.name, room: v.room, participant_identity: v.participant_identity, participant_name: v.participant_name },
      });
      await qc.invalidateQueries({ queryKey: [projectId, "media", "ingress"] });
      form.reset(v);
      toast("Ingress saved");
    } catch (err) {
      setError(applyApiError(form, err, { room_name: "room" }));
    }
  });
  const { errors, isSubmitting, isDirty } = form.formState;
  return (
    <Panel title="Settings">
      <form onSubmit={submit} className="flex max-w-lg flex-col gap-3 p-3">
        <Field label="Name" error={errors.name?.message}>
          <Input {...form.register("name", { required: "Give it a name." })} />
        </Field>
        <Field label="Room" error={errors.room?.message}>
          <Input {...form.register("room", { required: "Say which room to publish into." })} />
        </Field>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Participant identity" error={errors.participant_identity?.message}>
            <Input {...form.register("participant_identity", { required: "Give the stream an identity." })} />
          </Field>
          <Field label="Display name" optional>
            <Input {...form.register("participant_name")} />
          </Field>
        </div>
        {error && (
          <p role="alert" className="text-sm text-critical-text">
            {error}
          </p>
        )}
        <div>
          <Button type="submit" variant="primary" pending={isSubmitting} disabled={!isDirty}>
            Save changes
          </Button>
        </div>
      </form>
    </Panel>
  );
}

export default function IngressDetailPage() {
  const id = decodeURIComponent(useParams<{ id: string }>().id);
  const path = `/ingress/${encodeURIComponent(id)}`;
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const router = useRouter();
  const now = useNow(1000);
  const [dialog, setDialog] = React.useState<"rotate" | "delete" | null>(null);
  const [revealed, setRevealed] = React.useState<Ingress | null>(null);
  const query = useProjectQuery<Ingress>(["media", "ingress", "one", id], path, { refetchInterval: (q) => (isRouteMissing(q.state.error) ? 30_000 : 2000) });
  const logs = useProjectQuery<Items<LogEvent>>(["media", "ingress", "logs", id], `${path}/logs`, { refetchInterval: (q) => (q.state.error ? 30_000 : 5000), enabled: !query.error });
  const invalidate = () => qc.invalidateQueries({ queryKey: [projectId, "media", "ingress"] });
  const setEnabled = useMutation<Ingress, ApiError, boolean>({
    mutationFn: (enabled) => call<Ingress>(path, { method: "PATCH", body: { enabled } }),
    onSuccess: invalidate,
    onError: (err) => toastError(err, "Could not change that"),
  });
  const stop = useMutation<unknown, ApiError>({
    mutationFn: () => call(`${path}/stop`, { method: "POST" }),
    onSuccess: () => {
      toast("Stream stopped", "The ingress can be used again.");
      return invalidate();
    },
    onError: (err) => toastError(err, "Could not stop the stream"),
  });

  const back = (
    <Link href="/ingress" className="link inline-flex items-center gap-1 text-sm text-ink-2">
      <ArrowLeft className="size-3" aria-hidden /> All ingresses
    </Link>
  );

  if (query.error) {
    return (
      <Page title="Ingress" description={back}>
        <Panel>
          {isRouteMissing(query.error) ? (
            <ServiceNotRunning name="ingress" />
          ) : query.error.status === 404 ? (
            <EmptyState title="This ingress does not exist">
              <p>It may have been deleted.</p>
            </EmptyState>
          ) : (
            <ErrorState error={query.error} onRetry={() => query.refetch()} />
          )}
        </Panel>
      </Page>
    );
  }

  const i = query.data;
  const live = i && (i.status === "publishing" || i.status === "buffering");
  return (
    <Page
      title={
        <span className="flex flex-wrap items-center gap-2">
          {i?.name ?? "Ingress"} {i && <IngressStatusBadge status={i.status} />}
        </span>
      }
      description={back}
      actions={
        i && (
          <>
            {live && (
              <Button variant="danger" pending={stop.isPending} onClick={() => stop.mutate()}>
                <Square aria-hidden /> Stop stream
              </Button>
            )}
            {i.input_type !== "url" && (
              <Button onClick={() => setDialog("rotate")}>
                <KeyRound aria-hidden /> Rotate stream key
              </Button>
            )}
            <Button variant="danger" onClick={() => setDialog("delete")}>
              <Trash2 aria-hidden /> Delete
            </Button>
          </>
        )
      }
    >
      {i?.status === "error" && i.error && (
        <Callout tone="critical" title="The stream failed">
          {i.error}
        </Callout>
      )}
      {i?.status === "inactive" && i.input_type !== "whip" && <IngressWorkerNotice />}
      {i?.status === "inactive" && (
        <p className="text-sm text-ink-2">
          No stream is arriving. If your encoder is pushing and this stays put, check its address and stream key, and the{" "}
          <Link className="link" href="/servers">
            Servers page
          </Link>
          .
        </p>
      )}
      <Panel title="Connection">
        {!i ? (
          <div className="p-3">
            <Skeleton className="h-16" />
          </div>
        ) : (
          <Facts
            items={[
              { label: "ID", value: <CopyValue value={i.id} /> },
              { label: "Input", value: INPUT_LABELS[i.input_type] ?? i.input_type },
              ...(i.input_type === "url"
                ? [{ label: "Pulling from", value: <CopyValue value={i.source_url} />, wide: true }]
                : [
                    { label: i.input_type === "whip" ? "WHIP endpoint" : "Publish to", value: <CopyValue value={i.publish_url} />, wide: true },
                    { label: "Stream key", value: <span className="text-ink-3">Shown once. Rotate it to get a new one.</span> },
                  ]),
              {
                label: "Enabled",
                value: (
                  <span className="inline-flex items-center gap-2">
                    <Switch checked={i.enabled} disabled={setEnabled.isPending} aria-label="Enabled" onCheckedChange={(v) => setEnabled.mutate(v)} />
                    {i.enabled ? <StatusBadge tone="good">On</StatusBadge> : <StatusBadge tone="neutral">Off</StatusBadge>}
                  </span>
                ),
              },
              { label: "Created", value: formatDateTime(i.created_at) },
              { label: "Last changed", value: formatDateTime(i.updated_at) },
            ]}
          />
        )}
      </Panel>
      <Panel title="Stream health">
        {!i ? (
          <div className="p-3">
            <Skeleton className="h-12" />
          </div>
        ) : !live ? (
          <EmptyState title="No stream right now" className="py-6">
            <p>Bitrate, resolution, frame rate and codecs show here while a stream is arriving.</p>
          </EmptyState>
        ) : (
          <Facts
            items={[
              { label: "Bitrate in", value: formatBitrate(i.stats.bitrate_in), testId: "ingress-bitrate" },
              { label: "Resolution", value: i.stats.width ? `${i.stats.width}×${i.stats.height}` : "Unknown" },
              { label: "Frame rate", value: i.stats.fps ? `${formatDecimal(i.stats.fps)} fps` : "Unknown" },
              { label: "Video codec", value: i.stats.video_codec || "None" },
              { label: "Audio codec", value: i.stats.audio_codec || "None" },
              { label: "Live for", value: i.stats.started_at ? formatDuration((now - Date.parse(i.stats.started_at)) / 1000) : "–" },
            ]}
          />
        )}
      </Panel>
      {i && <EditPanel ingress={i} />}
      <Panel title="Log" description="From the event log, newest first.">
        {logs.isPending ? (
          <div className="p-3">
            <Skeleton className="h-12" />
          </div>
        ) : logs.error ? (
          <ErrorState error={logs.error} onRetry={() => logs.refetch()} />
        ) : logs.data.items.length === 0 ? (
          <EmptyState title="No log entries yet" className="py-6" />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Time</Th>
                <Th>Message</Th>
              </tr>
            </thead>
            <tbody>
              {logs.data.items.map((e) => (
                <Tr key={e.id}>
                  <Td className="num text-xs whitespace-nowrap text-ink-2" title={formatShortTime(e.created_at)}>
                    {formatClock(e.created_at)}
                  </Td>
                  <Td>{e.message}</Td>
                </Tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <StreamKeyReveal ingress={revealed} onDone={() => setRevealed(null)} />
      <ConfirmDialog
        open={dialog === "rotate"}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Rotate the stream key?"
        description="The old key stops working, and you get a new one to paste into your encoder."
        confirmLabel="Rotate key"
        onConfirm={async () => {
          const next = await call<Ingress>(`${path}/rotate-key`, { method: "POST" });
          await invalidate();
          setRevealed(next);
        }}
      />
      <ConfirmDialog
        open={dialog === "delete"}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Delete ${i?.name ?? "this ingress"}?`}
        description="This stops any running stream, and the stream key stops working."
        confirmLabel="Delete ingress"
        onConfirm={async () => {
          await call(path, { method: "DELETE" });
          await invalidate();
          toast("Ingress deleted");
          router.push("/ingress");
        }}
      />
    </Page>
  );
}
