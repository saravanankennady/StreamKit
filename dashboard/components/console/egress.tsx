"use client";

import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { Callout, StatusBadge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { CheckRow } from "@/components/ui/switch";
import { api, ApiError, isRouteMissing } from "@/lib/api";
import { applyApiError } from "@/lib/form";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { EgressJob, EgressStatus, EgressType, Items, Page, RoomStat } from "@/lib/types";

const statuses: Record<EgressStatus, { tone: Tone; label: string }> = {
  starting: { tone: "neutral", label: "Starting" },
  active: { tone: "good", label: "Active" },
  ending: { tone: "warn", label: "Ending" },
  complete: { tone: "good", label: "Complete" },
  failed: { tone: "critical", label: "Failed" },
  aborted: { tone: "serious", label: "Aborted" },
};

export function EgressStatusBadge({ status }: { status: EgressStatus }) {
  const s = statuses[status] ?? { tone: "neutral" as const, label: status };
  return <StatusBadge tone={s.tone}>{s.label}</StatusBadge>;
}

export const EGRESS_TYPES: { value: EgressType; label: string }[] = [
  { value: "room_composite", label: "Whole room, composed" },
  { value: "participant", label: "One participant" },
  { value: "audio_only", label: "Audio only" },
  { value: "video_only", label: "Video only" },
];

export const egressTypeLabel = (t: string) => EGRESS_TYPES.find((x) => x.value === t)?.label ?? t;

export const isRunning = (s: EgressStatus) => s === "starting" || s === "active" || s === "ending";

/** Pages through a job list, and keeps checking while the backend has no such route, so it starts working when it does. */
export function useJobList(key: string, path: string) {
  const { projectId } = useProject();
  return useInfiniteQuery<Page<EgressJob>, ApiError>({
    queryKey: [projectId, "media", key, "list"],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) => {
      const q = new URLSearchParams({ limit: "50" });
      if (pageParam) q.set("cursor", pageParam as string);
      return api<Page<EgressJob>>(`${path}?${q}`, { project: projectId, signal });
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
    refetchInterval: (q) => (isRouteMissing(q.state.error) ? 30_000 : 3000),
  });
}

interface StartForm {
  type: EgressType;
  room: string;
  participant: string;
  layout: string;
  file: boolean;
  format: string;
  stream_urls: string;
}

/** Starts an egress job. A recording is the same thing with a file output, so both pages use this. */
export function StartEgressDialog({ open, onOpenChange, recording }: { open: boolean; onOpenChange: (open: boolean) => void; recording?: boolean }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const rooms = useProjectQuery<Items<RoomStat>>(["rooms", "open"], "/rooms", { enabled: open });
  const defaults: StartForm = { type: "room_composite", room: "", participant: "", layout: "grid", file: true, format: "mp4", stream_urls: "" };
  const form = useForm<StartForm>({ defaultValues: defaults });
  const [error, setError] = React.useState<{ message: string; noWorker: boolean } | null>(null);

  React.useEffect(() => {
    if (open) {
      form.reset(defaults);
      setError(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const type = form.watch("type");
  const file = form.watch("file");

  const submit = form.handleSubmit(async (v) => {
    setError(null);
    const stream_urls = v.stream_urls.split("\n").map((s) => s.trim()).filter(Boolean);
    if (!recording && !v.file && stream_urls.length === 0) {
      form.setError("stream_urls", { message: "Choose a file output, a stream URL, or both." });
      return;
    }
    try {
      await call<EgressJob>("/egress", {
        method: "POST",
        body: {
          type: v.type,
          room: v.room,
          participant: v.participant || undefined,
          layout: v.type === "room_composite" ? v.layout : undefined,
          output: v.file || recording ? { kind: "file", format: v.format } : undefined,
          stream_urls: stream_urls.length ? stream_urls : undefined,
        },
      });
      await qc.invalidateQueries({ queryKey: [projectId, "media"] });
      onOpenChange(false);
    } catch (err) {
      setError({ message: applyApiError(form, err) ?? "", noWorker: err instanceof ApiError && err.code === "egress_unavailable" });
    }
  });

  const { errors, isSubmitting } = form.formState;
  const roomOptions = (rooms.data?.items ?? []).map((r) => r.name);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title={recording ? "Start a recording" : "Start an egress job"} description={recording ? "Records an open room to a file on this server." : "Sends an open room to a file, to streaming URLs, or to both."}>
        <form onSubmit={submit}>
          <DialogBody>
            <Field label="What to capture">
              <Controller control={form.control} name="type" render={({ field }) => <Select value={field.value} onValueChange={field.onChange} options={EGRESS_TYPES} />} />
            </Field>
            <Field label="Room" hint={roomOptions.length ? `Open now: ${roomOptions.slice(0, 5).join(", ")}${roomOptions.length > 5 ? " and more" : ""}` : "No rooms are open right now."} error={errors.room?.message}>
              <Input {...form.register("room", { required: "Name the room to capture." })} />
            </Field>
            {(type === "participant" || type === "audio_only" || type === "video_only") && (
              <Field
                label="Participant identity"
                optional={type !== "participant"}
                hint={type === "participant" ? undefined : "Leave blank for the whole room."}
                error={errors.participant?.message}
              >
                <Input {...form.register("participant", { validate: (v, all) => all.type !== "participant" || !!v.trim() || "Name the participant to capture." })} />
              </Field>
            )}
            {type === "room_composite" && (
              <Field label="Layout">
                <Controller
                  control={form.control}
                  name="layout"
                  render={({ field }) => (
                    <Select
                      value={field.value}
                      onValueChange={field.onChange}
                      options={[
                        { value: "grid", label: "Grid" },
                        { value: "speaker", label: "Active speaker" },
                      ]}
                    />
                  )}
                />
              </Field>
            )}
            {!recording && <Controller control={form.control} name="file" render={({ field }) => <CheckRow label="Save to a file" hint="It also appears under Recordings." checked={field.value} onCheckedChange={field.onChange} />} />}
            {(file || recording) && (
              <Field label="File format" hint="WebM is available where the codec allows it.">
                <Controller
                  control={form.control}
                  name="format"
                  render={({ field }) => (
                    <Select
                      value={field.value}
                      onValueChange={field.onChange}
                      options={[
                        { value: "mp4", label: "MP4" },
                        { value: "webm", label: "WebM" },
                      ]}
                    />
                  )}
                />
              </Field>
            )}
            {!recording && (
              <Field label="Stream to" optional hint="One URL per line, like rtmp://a.rtmp.youtube.com/live2/your-key" error={errors.stream_urls?.message}>
                <Textarea rows={2} className="min-h-14 font-mono text-xs" {...form.register("stream_urls")} />
              </Field>
            )}
            {error?.noWorker ? (
              <Callout tone="serious" title="No egress worker is available">
                {error.message} Nothing was started. Check the worker&rsquo;s health on the Servers page, then try again.
              </Callout>
            ) : error?.message ? (
              <p role="alert" className="text-sm text-critical-text">
                {error.message}
              </p>
            ) : null}
          </DialogBody>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" variant="primary" pending={isSubmitting}>
              {recording ? "Start recording" : "Start job"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
