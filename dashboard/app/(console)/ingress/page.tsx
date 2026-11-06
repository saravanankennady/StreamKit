"use client";

import { useQueryClient } from "@tanstack/react-query";
import { Plus, Upload } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import * as React from "react";
import { Controller, useForm } from "react-hook-form";
import { INPUT_LABELS, IngressStatusBadge, StreamKeyReveal } from "@/components/console/ingress";
import { EmptyState, ErrorState, Page, Panel, ServiceNotRunning } from "@/components/console/page";
import { IngressWorkerNotice } from "@/components/console/workers";
import { StatusBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { TableSkeleton } from "@/components/ui/skeleton";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { isRouteMissing } from "@/lib/api";
import { formatBitrate } from "@/lib/format";
import { applyApiError } from "@/lib/form";
import { useProject, useProjectApi, useProjectQuery } from "@/lib/session";
import type { Ingress, IngressInput, Items } from "@/lib/types";

interface CreateForm {
  name: string;
  input_type: IngressInput;
  room: string;
  participant_identity: string;
  participant_name: string;
  source_url: string;
}

function CreateIngressDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: (i: Ingress) => void }) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  const form = useForm<CreateForm>({ defaultValues: { name: "", input_type: "rtmp", room: "", participant_identity: "", participant_name: "", source_url: "" } });
  const [error, setError] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (open) {
      form.reset();
      setError(null);
    }
  }, [open, form]);
  const input = form.watch("input_type");
  const submit = form.handleSubmit(async (v) => {
    setError(null);
    try {
      const created = await call<Ingress>("/ingress", {
        method: "POST",
        body: {
          name: v.name,
          input_type: v.input_type,
          room: v.room,
          participant_identity: v.participant_identity,
          participant_name: v.participant_name || undefined,
          source_url: v.input_type === "url" ? v.source_url : undefined,
        },
      });
      await qc.invalidateQueries({ queryKey: [projectId, "media", "ingress"] });
      onOpenChange(false);
      onCreated(created);
    } catch (err) {
      setError(applyApiError(form, err, { room_name: "room" }));
    }
  });
  const { errors, isSubmitting } = form.formState;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Create an ingress" description="Brings an outside stream into a room, as a participant. This only saves the configuration. Nothing connects until a stream arrives.">
        <form onSubmit={submit}>
          <DialogBody>
            <Field label="Name" error={errors.name?.message}>
              <Input autoFocus placeholder="Main stage" {...form.register("name", { required: "Give it a name." })} />
            </Field>
            <Field label="Input">
              <Controller
                control={form.control}
                name="input_type"
                render={({ field }) => (
                  <Select
                    value={field.value}
                    onValueChange={field.onChange}
                    options={[
                      { value: "rtmp", label: "RTMP: OBS and other encoders push to us" },
                      { value: "whip", label: "WHIP: WebRTC encoders push to us" },
                      { value: "url", label: "URL: we pull from an HTTP or SRT source" },
                    ]}
                  />
                )}
              />
            </Field>
            {input === "url" && (
              <Field label="Source URL" hint="https:// or srt://" error={errors.source_url?.message}>
                <Input className="font-mono text-xs" {...form.register("source_url", { validate: (v, all) => all.input_type !== "url" || !!v.trim() || "Enter the URL to pull from." })} />
              </Field>
            )}
            <Field label="Room" error={errors.room?.message}>
              <Input {...form.register("room", { required: "Say which room to publish into." })} />
            </Field>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Participant identity" error={errors.participant_identity?.message}>
                <Input placeholder="stage" {...form.register("participant_identity", { required: "Give the stream an identity in the room." })} />
              </Field>
              <Field label="Display name" optional error={errors.participant_name?.message}>
                <Input placeholder="Main stage" {...form.register("participant_name")} />
              </Field>
            </div>
            {error && (
              <p role="alert" className="text-sm text-critical-text">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" variant="primary" pending={isSubmitting}>
              Create ingress
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export default function IngressPage() {
  const list = useProjectQuery<Items<Ingress>>(["media", "ingress", "list"], "/ingress", { refetchInterval: (q) => (isRouteMissing(q.state.error) ? 30_000 : 3000) });
  const [creating, setCreating] = React.useState(false);
  const [revealed, setRevealed] = React.useState<Ingress | null>(null);
  const router = useRouter();
  const missing = isRouteMissing(list.error);
  const items = list.data?.items ?? [];
  return (
    <Page
      title="Ingress"
      description="Bring an outside stream into a room: push from OBS over RTMP or WHIP, or have the server pull from a URL."
      actions={
        !missing && (
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus aria-hidden /> Create ingress
          </Button>
        )
      }
    >
      {!missing && !list.isPending && <IngressWorkerNotice />}
      <Panel>
        {list.isPending ? (
          <TableSkeleton rows={3} />
        ) : missing ? (
          <ServiceNotRunning name="ingress" />
        ) : list.error ? (
          <ErrorState error={list.error} onRetry={() => list.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState icon={Upload} title="No ingresses yet" action={<Button variant="primary" onClick={() => setCreating(true)}>Create ingress</Button>}>
            <p>Create one to get a publish address and a stream key to paste into your encoder.</p>
          </EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Name</Th>
                <Th className="hidden sm:table-cell">Input</Th>
                <Th>Room</Th>
                <Th className="hidden md:table-cell">Identity</Th>
                <Th>Status</Th>
                <Th className="hidden text-right lg:table-cell">Bitrate</Th>
                <Th className="hidden lg:table-cell">Enabled</Th>
              </tr>
            </thead>
            <tbody>
              {items.map((i) => (
                <Tr key={i.id} data-testid="ingress-row" data-id={i.id} data-status={i.status}>
                  <Td className="max-w-52">
                    <Link className="link block truncate font-medium" href={`/ingress/${encodeURIComponent(i.id)}`}>
                      {i.name}
                    </Link>
                  </Td>
                  <Td className="hidden whitespace-nowrap text-ink-2 sm:table-cell">{INPUT_LABELS[i.input_type] ?? i.input_type}</Td>
                  <Td className="max-w-40 truncate">{i.room_name}</Td>
                  <Td className="hidden max-w-40 truncate text-ink-2 md:table-cell">{i.participant_identity}</Td>
                  <Td>
                    <IngressStatusBadge status={i.status} />
                  </Td>
                  <Td className="num hidden text-right whitespace-nowrap lg:table-cell">{i.status === "publishing" ? formatBitrate(i.stats.bitrate_in) : "–"}</Td>
                  <Td className="hidden lg:table-cell">{i.enabled ? <StatusBadge tone="good">On</StatusBadge> : <StatusBadge tone="neutral">Off</StatusBadge>}</Td>
                </Tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <CreateIngressDialog open={creating} onOpenChange={setCreating} onCreated={(i) => (i.stream_key ? setRevealed(i) : router.push(`/ingress/${encodeURIComponent(i.id)}`))} />
      <StreamKeyReveal
        ingress={revealed}
        onDone={() => {
          const id = revealed?.id;
          setRevealed(null);
          if (id) router.push(`/ingress/${encodeURIComponent(id)}`);
        }}
      />
    </Page>
  );
}
