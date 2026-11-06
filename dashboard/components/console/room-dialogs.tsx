"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import * as React from "react";
import { useForm } from "react-hook-form";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Dialog, DialogBody, DialogContent, DialogFooter } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { toast } from "@/components/ui/toast";
import { ApiError } from "@/lib/api";
import { applyApiError, optionalNumber } from "@/lib/form";
import { useProject, useProjectApi } from "@/lib/session";
import type { RoomStat } from "@/lib/types";

interface CreateForm {
  name: string;
  metadata: string;
  empty_timeout: string;
  departure_timeout: string;
  max_participants: string;
}

export function CreateRoomDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { project, projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const router = useRouter();
  const form = useForm<CreateForm>({ defaultValues: { name: "", metadata: "", empty_timeout: "", departure_timeout: "", max_participants: "" } });
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      form.reset();
      setError(null);
    }
  }, [open, form]);

  const submit = form.handleSubmit(async (v) => {
    setError(null);
    try {
      const room = await call<RoomStat>("/rooms", {
        method: "POST",
        body: {
          name: v.name,
          metadata: v.metadata || undefined,
          empty_timeout: optionalNumber(v.empty_timeout),
          departure_timeout: optionalNumber(v.departure_timeout),
          max_participants: optionalNumber(v.max_participants),
        },
      });
      await qc.invalidateQueries({ queryKey: [projectId, "rooms"] });
      onOpenChange(false);
      toast(`Room ${room.name} is open`);
      router.push(`/rooms/${encodeURIComponent(room.name)}`);
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  const s = project.settings;
  const { errors, isSubmitting } = form.formState;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Create a room" description="If a room with this name is already open, you get that room.">
        <form onSubmit={submit}>
          <DialogBody>
            <Field label="Name" error={errors.name?.message}>
              <Input autoFocus placeholder="standup" {...form.register("name", { required: "Give the room a name." })} />
            </Field>
            <Field label="Metadata" optional hint="Any string. Everyone in the room can read it." error={errors.metadata?.message}>
              <Textarea rows={2} className="min-h-14 font-mono text-xs" {...form.register("metadata")} />
            </Field>
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label="Empty timeout" optional hint={`Seconds. Default ${s.empty_timeout ?? "from the server"}.`} error={errors.empty_timeout?.message}>
                <Input type="number" min={0} inputMode="numeric" {...form.register("empty_timeout")} />
              </Field>
              <Field label="Departure timeout" optional hint={`Seconds. Default ${s.departure_timeout ?? "from the server"}.`} error={errors.departure_timeout?.message}>
                <Input type="number" min={0} inputMode="numeric" {...form.register("departure_timeout")} />
              </Field>
              <Field label="Max participants" optional hint={s.max_participants ? `Default ${s.max_participants}.` : "Default no limit."} error={errors.max_participants?.message}>
                <Input type="number" min={0} inputMode="numeric" {...form.register("max_participants")} />
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
              Create room
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function EditMetadataDialog({ room, open, onOpenChange }: { room: RoomStat; open: boolean; onOpenChange: (open: boolean) => void }) {
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  const [value, setValue] = React.useState(room.metadata);
  const id = React.useId();
  React.useEffect(() => {
    if (open) setValue(room.metadata);
  }, [open, room.metadata]);
  const save = useMutation<RoomStat, ApiError>({
    mutationFn: () => call<RoomStat>(`/rooms/${encodeURIComponent(room.name)}`, { method: "PATCH", body: { metadata: value } }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: [projectId, "rooms"] });
      onOpenChange(false);
      toast("Metadata updated", "Everyone in the room was told.");
    },
  });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title={`Metadata for ${room.name}`} description="Replaces what is there now. Everyone in the room can read it.">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <DialogBody>
            <Field label="Metadata" error={save.error?.message}>
              <Textarea id={id} rows={5} className="font-mono text-xs" value={value} onChange={(e) => setValue(e.target.value)} />
            </Field>
          </DialogBody>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" variant="primary" pending={save.isPending}>
              Save metadata
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function CloseRoomDialog({ room, open, onOpenChange, onClosed }: { room: { name: string; participants: number }; open: boolean; onOpenChange: (open: boolean) => void; onClosed?: () => void }) {
  const { projectId } = useProject();
  const call = useProjectApi();
  const qc = useQueryClient();
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Close ${room.name}?`}
      description={
        room.participants > 0
          ? `This disconnects ${room.participants === 1 ? "the 1 person" : `all ${room.participants} people`} in the room straight away.`
          : "Nobody is in this room. It moves to history."
      }
      confirmLabel="Close room"
      onConfirm={async () => {
        await call(`/rooms/${encodeURIComponent(room.name)}`, { method: "DELETE" });
        await qc.invalidateQueries({ queryKey: [projectId, "rooms"] });
        toast(`Room ${room.name} closed`);
        onClosed?.();
      }}
    />
  );
}
